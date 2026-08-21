package mcpserver

import (
	"context"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/failuresig"
	runmodel "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
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
			wantActions:  []string{"approve_pr", "fishhawk_merge_run"},
			wantConsumes: []string{consumesNone, consumesNone},
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
			wantActions:  []string{"approve_pr", "fishhawk_merge_run"},
			wantConsumes: []string{consumesNone, consumesNone},
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
			na := nextActionsFor(tc.run, tc.stages, tc.planRS, tc.implRS, tc.hint, nil, false, false, false, "", "", releaseSignals{})
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

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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

// TestNextActions_HostDispatchClassification pins the #1912 routing (plan test
// m): a stage at awaiting_host_dispatch routes to the DISPATCH arm (the operator
// host spawns it), while a bare 'dispatched' stage (a spawn attempt exists —
// in-flight) routes to POLL-only, across the plan, implement, and acceptance
// arms.
func TestNextActions_HostDispatchClassification(t *testing.T) {
	cases := []struct {
		name       string
		stages     []Stage
		wantState  string
		wantAction string // "" means poll-only (a pollAction first entry)
	}{
		{
			name:       "implement_awaiting_host_dispatch_routes_to_dispatch",
			stages:     []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_host_dispatch")},
			wantState:  "implement_pending",
			wantAction: "fishhawk_dispatch_stage",
		},
		{
			name:      "implement_dispatched_routes_to_poll",
			stages:    []Stage{naStage("plan", "succeeded"), naStage("implement", "dispatched")},
			wantState: "implement_dispatched",
		},
		{
			name:       "plan_awaiting_host_dispatch_routes_to_dispatch",
			stages:     []Stage{naStage("plan", "awaiting_host_dispatch")},
			wantState:  "plan_pending",
			wantAction: "fishhawk_run_stage",
		},
		{
			name:      "plan_dispatched_routes_to_poll",
			stages:    []Stage{naStage("plan", "dispatched")},
			wantState: "plan_dispatched",
		},
		{
			name:       "acceptance_awaiting_host_dispatch_routes_to_dispatch",
			stages:     []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded"), naStage("acceptance", "awaiting_host_dispatch")},
			wantState:  "acceptance_pending",
			wantAction: "fishhawk_dispatch_stage",
		},
		{
			name:      "acceptance_dispatched_routes_to_poll",
			stages:    []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded"), naStage("acceptance", "dispatched")},
			wantState: "acceptance_dispatched",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := naRun("running")
			na := nextActionsFor(run, tc.stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
			if na == nil || na.State != tc.wantState {
				t.Fatalf("state = %+v, want %q", na, tc.wantState)
			}
			if len(na.Actions) == 0 {
				t.Fatalf("no actions for %q (non-terminal must carry >=1)", tc.wantState)
			}
			if tc.wantAction != "" {
				if na.Actions[0].Action != tc.wantAction {
					t.Errorf("actions[0] = %q, want %q (dispatch arm)", na.Actions[0].Action, tc.wantAction)
				}
			} else {
				// poll-only: the first action is a re-poll (fishhawk_get_run_status),
				// never a dispatch/run verb.
				if na.Actions[0].Action != "fishhawk_get_run_status" {
					t.Errorf("actions[0] = %q, want fishhawk_get_run_status (poll-only for a bare dispatched stage)", na.Actions[0].Action)
				}
			}
		})
	}
}

// TestNextActions_PlanLocalDispatchUnchanged pins condition (1): the
// plan-local branch is byte-unchanged — a parked LOCAL plan stage still
// offers the single fishhawk_run_stage action and never dispatch_stage.
func TestNextActions_PlanLocalDispatchUnchanged(t *testing.T) {
	run := naRun("pending")
	stages := []Stage{naStage("plan", "pending")}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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

			na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	if na == nil || na.State != "implement_awaiting_scope_decision" {
		t.Fatalf("state = %+v, want implement_awaiting_scope_decision", na)
	}
	dec := findAction(t, na, "fishhawk_decide_scope_completeness")
	if dec.Params["run_id"] != run.ID {
		t.Errorf("decide params = %v, want run_id=%s", dec.Params, run.ID)
	}
	if dec.Params["decision"] != "exempt|amend|fail" {
		t.Errorf("decide params = %v, want the exempt|amend|fail decision hint (#2591 added amend)", dec.Params)
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

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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
// different ways. It also pins the E48.7 / #1954 drive-folded merge_pr
// TRANSLATION: the server-stamped merge_pr next_action surfaces as
// fishhawk_merge_run at the MCP layer (with the run_id + verdict params),
// while the drive detail and PR URL carry through unchanged.
func TestNextActions_DriveActionFoldsFirst(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naRun("running")
	run.PullRequestURL = &prURL
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}
	// A QUALIFIED drive detail — the shape the backend now stamps when a
	// reviewer has rejected (#2487). The passthrough is what carries the
	// warning to the operator, so it must surface verbatim as the reason.
	const qualifiedDetail = "all blocking gates resolved and required checks are green; 2 advisory rejects (latest review round) outstanding — read them with fishhawk_get_gate_view, then merge, route a fix-up with fishhawk_fixup_stage, or waive with fishhawk_waive_concern"
	drive := &DriveStatus{
		Drive:      true,
		NextAction: &RunNextAction{Action: "merge_pr", Detail: qualifiedDetail, PRURL: prURL},
	}

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, drive, false, false, false, "", "", releaseSignals{})
	if na == nil || len(na.Actions) == 0 {
		t.Fatalf("nextActionsFor = %+v, want the drive action folded in first", na)
	}
	if na.Actions[0].Action != "fishhawk_merge_run" {
		t.Errorf("actions[0] = %q, want the drive next_action merge_pr translated to fishhawk_merge_run", na.Actions[0].Action)
	}
	if na.Actions[0].Reason != qualifiedDetail {
		t.Errorf("actions[0].reason = %q, want the qualified drive detail verbatim", na.Actions[0].Reason)
	}
	// #2487: the precondition no longer makes the unconditional all-clear
	// claim — the qualification is carried by the passed-through detail.
	if strings.Contains(na.Actions[0].Precondition, "every gate resolved and required checks green") {
		t.Errorf("actions[0].precondition = %q, want it to drop the unconditional all-clear claim (#2487)", na.Actions[0].Precondition)
	}
	if na.Actions[0].Params["pr_url"] != prURL {
		t.Errorf("actions[0].params.pr_url = %q, want %q", na.Actions[0].Params["pr_url"], prURL)
	}
	if na.Actions[0].Params["run_id"] != run.ID {
		t.Errorf("actions[0].params.run_id = %q, want %q", na.Actions[0].Params["run_id"], run.ID)
	}
	if _, ok := na.Actions[0].Params["verdict"]; !ok {
		t.Errorf("actions[0].params missing verdict placeholder: %v", na.Actions[0].Params)
	}
}

// TestNextActions_DriveActionNonMergePassesThrough pins that a NON-merge
// drive next_action (e.g. run_implement_stage) is folded in verbatim — the
// merge_pr translation is scoped strictly to the merge verb.
func TestNextActions_DriveActionNonMergePassesThrough(t *testing.T) {
	run := naRun("running")
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "pending")}
	drive := &DriveStatus{
		Drive:      true,
		NextAction: &RunNextAction{Action: "run_implement_stage", Detail: "dispatch it"},
	}
	na := nextActionsFor(run, stages, nil, nil, nil, drive, false, false, false, "", "", releaseSignals{})
	if na == nil || len(na.Actions) == 0 {
		t.Fatalf("nextActionsFor = %+v, want the drive action folded in first", na)
	}
	if na.Actions[0].Action != "run_implement_stage" {
		t.Errorf("actions[0] = %q, want the non-merge drive action passed through verbatim", na.Actions[0].Action)
	}
	if _, ok := na.Actions[0].Params["verdict"]; ok {
		t.Errorf("actions[0] should carry no verdict param for a non-merge action: %v", na.Actions[0].Params)
	}
}

// TestNextActions_CategoryAFlakeCitation pins the best-effort flake
// citation: with the verify_infra_flake_retry signature in the failure
// detail the retry reason cites it; without it the retry action is
// still emitted, uncited.
func TestNextActions_CategoryAFlakeCitation(t *testing.T) {
	run := naRun("failed")
	cited := nextActionsFor(run, []Stage{naStage("plan", "succeeded"),
		naFailedImplement("A", "verify aborted after verify_infra_flake_retry")}, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	retry := findAction(t, cited, "fishhawk_retry_stage")
	if !strings.Contains(retry.Reason, "verify_infra_flake_retry") {
		t.Errorf("retry reason should cite the flake trace event; got %q", retry.Reason)
	}

	uncited := nextActionsFor(run, []Stage{naStage("plan", "succeeded"),
		naFailedImplement("A", "agent crashed")}, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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
		nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	retry := findAction(t, cited, "fishhawk_retry_stage")
	if !strings.Contains(retry.Reason, "529") {
		t.Errorf("retry reason should name the 529 status; got %q", retry.Reason)
	}
	if !strings.Contains(retry.Reason, "status.claude.com") {
		t.Errorf("retry reason should point at status.claude.com; got %q", retry.Reason)
	}

	// A plain category-A failure keeps the generic reason and names no status.
	uncited := nextActionsFor(run, []Stage{naStage("plan", "succeeded"),
		naFailedImplement("A", "agent crashed")}, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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
				nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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

// TestNextActions_CategoryAQuotaCitation pins the #2085 quota hint: a
// category-A failure whose reason carries the runner's stable "could not obtain
// model quota" phrase steers the operator to wait for the cap to reset (and
// says it is not a transient crash) rather than burning retry budget; a plain
// category-A failure keeps the generic retry reason. This locks the parse end
// of the runner->next_actions FailureReason string contract (the emit end is
// locked in the runner's claudecode/main tests).
func TestNextActions_CategoryAQuotaCitation(t *testing.T) {
	run := naRun("failed")
	cited := nextActionsFor(run, []Stage{naStage("plan", "succeeded"),
		naFailedImplement("A", "could not obtain model quota (likely a usage/rate cap): agent exited with exit status 1 after 2s having made no model call (0 tokens)")},
		nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	retry := findAction(t, cited, "fishhawk_retry_stage")
	if !strings.Contains(retry.Reason, "wait for the cap to reset") {
		t.Errorf("retry reason should tell the operator to wait for the cap to reset; got %q", retry.Reason)
	}
	if !strings.Contains(retry.Reason, "not a transient crash") {
		t.Errorf("retry reason should distinguish quota from a transient crash; got %q", retry.Reason)
	}

	// A plain category-A failure keeps the generic reason and names no quota cap.
	uncited := nextActionsFor(run, []Stage{naStage("plan", "succeeded"),
		naFailedImplement("A", "agent crashed")}, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	retry = findAction(t, uncited, "fishhawk_retry_stage")
	if strings.Contains(retry.Reason, "wait for the cap to reset") || strings.Contains(retry.Reason, "model quota") {
		t.Errorf("generic category-A retry reason must not cite a quota cap: %q", retry.Reason)
	}
}

// TestCitedQuotaUnavailable pins the nil-safe fingerprint helper: the stable
// quota phrase yields true; a nil stage, nil reason, or an unrelated reason
// yield false so the caller keeps the generic hint.
func TestCitedQuotaUnavailable(t *testing.T) {
	mk := func(reason string) *Stage {
		s := naStage("implement", "failed")
		if reason != "__nil__" {
			s.FailureReason = &reason
		}
		return &s
	}
	cases := []struct {
		name  string
		stage *Stage
		want  bool
	}{
		{"nil stage", nil, false},
		{"nil reason", mk("__nil__"), false},
		{"absent phrase", mk("agent exited with error: exit status 1"), false},
		{"quota phrase", mk("could not obtain model quota (likely a usage/rate cap): x"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := citedQuotaUnavailable(tc.stage); got != tc.want {
				t.Errorf("citedQuotaUnavailable = %v, want %v", got, tc.want)
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
		naFailedImplement("B", "undeclared created file")}, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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
		nil, naReviewStatus("implement", "pending"), nil, nil, false, false, false, "", "", releaseSignals{})
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
	if na := nextActionsFor(nil, nil, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{}); na != nil {
		t.Errorf("nextActionsFor(nil run) = %+v, want nil", na)
	}
}

// TestNextActions_PlanReviewPendingDoesNotOfferApproval pins the
// wait-for-the-agent-plan-review discipline: while the plan review is
// pending, the block must NOT contain an approval action.
func TestNextActions_PlanReviewPendingDoesNotOfferApproval(t *testing.T) {
	run := naRun("running")
	na := nextActionsFor(run, []Stage{naStage("plan", "awaiting_approval")},
		naReviewStatus("plan", "pending"), nil, nil, nil, false, false, false, "", "", releaseSignals{})
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

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), hint, drive, false, false, false, "", "", releaseSignals{})
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

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, drive, false, false, false, "", "", releaseSignals{})
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

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, drive, false, false, false, "", "", releaseSignals{})
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

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, nil, true, false, false, "", "", releaseSignals{})
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
// rewired approve_pr -> fishhawk_merge_run ritual (E48.7 / #1954).
func TestNextActions_SucceededPROpenUnchangedWhenMergeNotObserved(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naRun("succeeded")
	run.PullRequestURL = &prURL
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, nil, false, false, false, "", "", releaseSignals{})
	if na == nil || na.State != "succeeded_pr_open" {
		t.Fatalf("state = %+v, want succeeded_pr_open", na)
	}
	if got := actionNames(na); len(got) != 2 || got[0] != "approve_pr" || got[1] != "fishhawk_merge_run" {
		t.Fatalf("actions = %v, want [approve_pr fishhawk_merge_run]", got)
	}
	// The retired bare ritual steps are gone.
	for _, a := range na.Actions {
		if a.Action == "merge_pr" || a.Action == "post_merge" {
			t.Errorf("retired ritual step %q still surfaced — replaced by fishhawk_merge_run", a.Action)
		}
	}
}

// TestNextActions_AcceptanceNotValidated_AcknowledgementPrompt is the #2347
// BINDING-CONDITION-1 pin. The operator ruled that the acknowledgement is a
// PROMPT, not an enforcement — no merge block, no text-matching of operator
// prose (a gate that strands a run over verdict wording is a worse failure than
// the dishonest word being fixed). That ruling leaves the reason string in this
// arm as the ONE mechanism standing in for enforcement, and prose in a reason
// field can be silently deleted by a refactor with neither reviewer nor gate
// noticing. So the reason's two load-bearing claims are asserted directly:
//
//	(a) that ZERO criteria were verified, and
//	(b) that the operator's merge verdict should say so.
//
// If either claim stops being told to the operator, this test fails.
func TestNextActions_AcceptanceNotValidated_AcknowledgementPrompt(t *testing.T) {
	assertPrompt := func(t *testing.T, na *NextActions, wantState string) {
		t.Helper()
		if na == nil || na.State != wantState {
			t.Fatalf("state = %+v, want %s", na, wantState)
		}
		if got := actionNames(na); len(got) != 2 || got[0] != "approve_pr" || got[1] != "fishhawk_merge_run" {
			t.Fatalf("actions = %v, want the merge ritual [approve_pr fishhawk_merge_run] — not-validated is merge-ELIGIBLE", got)
		}
		reason := na.Actions[0].Reason
		// (a) zero criteria were verified.
		if !strings.Contains(reason, "ZERO") {
			t.Errorf("reason no longer tells the operator ZERO criteria were verified (#2347 condition 1):\n%s", reason)
		}
		// (b) the merge verdict should acknowledge it.
		if !strings.Contains(reason, "merge verdict") {
			t.Errorf("reason no longer asks the operator to acknowledge the non-validation in their merge verdict (#2347 condition 1):\n%s", reason)
		}
		if !strings.Contains(reason, "acknowledge") {
			t.Errorf("reason dropped the acknowledgement ask (#2347 condition 1):\n%s", reason)
		}
		// The prompt must not read as a pass.
		if strings.Contains(reason, "the acceptance stage passed") {
			t.Errorf("reason still reads as a validated pass:\n%s", reason)
		}
	}

	prURL := "https://github.com/x/y/pull/42"

	t.Run("running-run acceptance arm", func(t *testing.T) {
		r := naLocalRun("running")
		r.PullRequestURL = &prURL
		na := nextActionsFor(r, naAcceptanceStages("succeeded"), nil,
			naReviewStatus("implement", "complete"), nil, nil, false, false, false,
			acceptanceVerdictNotValidated, "", releaseSignals{})
		assertPrompt(t, na, "acceptance_not_validated")
	})

	t.Run("terminal-run arm", func(t *testing.T) {
		r := naRun("succeeded")
		r.PullRequestURL = &prURL
		stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}
		na := nextActionsFor(r, stages, nil, naReviewStatus("implement", "complete"), nil, nil,
			false, false, false, acceptanceVerdictNotValidated, "", releaseSignals{})
		assertPrompt(t, na, "succeeded_acceptance_not_validated")
	})

	// Degradation control: a verdict aged out of the recent-audit window (empty
	// string) leaves the terminal run on plain succeeded_pr_open — merge-eligible,
	// same as today. The label is what the signal gates, never the eligibility.
	t.Run("verdict aged out -> succeeded_pr_open", func(t *testing.T) {
		r := naRun("succeeded")
		r.PullRequestURL = &prURL
		stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}
		na := nextActionsFor(r, stages, nil, naReviewStatus("implement", "complete"), nil, nil,
			false, false, false, "", "", releaseSignals{})
		if na == nil || na.State != "succeeded_pr_open" {
			t.Fatalf("state = %+v, want succeeded_pr_open (graceful degradation)", na)
		}
	})

	// Precedence control: the out-of-scope skip flag is checked FIRST, so a run
	// carrying both signals keeps the skip label. The two short-circuit
	// predicates are disjoint in the orchestrator, so this only pins that the
	// new arm did not disturb the existing one.
	t.Run("skip flag still wins over the verdict signal", func(t *testing.T) {
		r := naRun("succeeded")
		r.PullRequestURL = &prURL
		stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}
		na := nextActionsFor(r, stages, nil, naReviewStatus("implement", "complete"), nil, nil,
			false, true, false, acceptanceVerdictNotValidated, "", releaseSignals{})
		if na == nil || na.State != "succeeded_acceptance_skipped_out_of_scope" {
			t.Fatalf("state = %+v, want succeeded_acceptance_skipped_out_of_scope", na)
		}
	})
}

// TestNextActions_AcceptanceUndecidable_AcknowledgementPrompt is the #2512 twin
// of the #2347 prompt pin above, and it exists for the same reason: the
// acknowledgement is a PROMPT, not an enforcement, so the reason string in these
// two arms is the ONE mechanism standing in for a gate — and prose in a reason
// field is exactly what a refactor deletes without anyone noticing.
//
// Three load-bearing claims are asserted on both arms:
//
//	(a) the stage could not DECIDE one or more criteria,
//	(b) the operator's merge verdict should say which, and
//	(c) it is NOT a triage — no criterion failed, so there is nothing to
//	    arbitrate. (c) is the claim that distinguishes this state from the
//	    failed/paged arm, and offering an arbitration here would send the
//	    operator to a verb the server refuses (the gate never reads
//	    acceptance_triage for an undecidable verdict, so no disposition exists
//	    to discharge).
func TestNextActions_AcceptanceUndecidable_AcknowledgementPrompt(t *testing.T) {
	assertPrompt := func(t *testing.T, na *NextActions, wantState string) {
		t.Helper()
		if na == nil || na.State != wantState {
			t.Fatalf("state = %+v, want %s", na, wantState)
		}
		if got := actionNames(na); len(got) != 2 || got[0] != "approve_pr" || got[1] != "fishhawk_merge_run" {
			t.Fatalf("actions = %v, want the merge ritual [approve_pr fishhawk_merge_run] — undecidable is merge-ELIGIBLE", got)
		}
		// No arbitration verb: an undecidable row is not a triageable defect.
		for _, n := range actionNames(na) {
			if n == "fishhawk_arbitrate_acceptance" {
				t.Errorf("actions offer fishhawk_arbitrate_acceptance — an undecidable verdict routes NO triage disposition to discharge (#2512): %v", actionNames(na))
			}
		}
		reason := na.Actions[0].Reason
		// (a) criteria could not be decided.
		if !strings.Contains(reason, "DECIDE") {
			t.Errorf("reason no longer tells the operator criteria could not be DECIDED (#2512):\n%s", reason)
		}
		// (b) the merge verdict should acknowledge it.
		if !strings.Contains(reason, "merge verdict") {
			t.Errorf("reason no longer asks the operator to acknowledge the undecided criteria in their merge verdict (#2512):\n%s", reason)
		}
		if !strings.Contains(reason, "acknowledge") {
			t.Errorf("reason dropped the acknowledgement ask (#2512):\n%s", reason)
		}
		// (c) it is not a triage.
		if !strings.Contains(reason, "not a triage") {
			t.Errorf("reason no longer says this is NOT a triage — the operator will look for an arbitration that does not exist (#2512):\n%s", reason)
		}
		// The prompt must not read as a pass.
		if strings.Contains(reason, "the acceptance stage passed") {
			t.Errorf("reason reads as a validated pass:\n%s", reason)
		}
	}

	prURL := "https://github.com/x/y/pull/42"

	t.Run("running-run acceptance arm", func(t *testing.T) {
		r := naLocalRun("running")
		r.PullRequestURL = &prURL
		na := nextActionsFor(r, naAcceptanceStages("succeeded"), nil,
			naReviewStatus("implement", "complete"), nil, nil, false, false, false,
			acceptanceVerdictUndecidable, "", releaseSignals{})
		assertPrompt(t, na, "acceptance_undecidable")
	})

	t.Run("terminal-run arm", func(t *testing.T) {
		r := naRun("succeeded")
		r.PullRequestURL = &prURL
		stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}
		na := nextActionsFor(r, stages, nil, naReviewStatus("implement", "complete"), nil, nil,
			false, false, false, acceptanceVerdictUndecidable, "", releaseSignals{})
		assertPrompt(t, na, "succeeded_acceptance_undecidable")
	})

	// Degradation control: a verdict aged out of the recent-audit window leaves
	// the terminal run on plain succeeded_pr_open — merge-eligible, same as
	// today. The label is what the signal gates, never the eligibility.
	t.Run("verdict aged out -> succeeded_pr_open", func(t *testing.T) {
		r := naRun("succeeded")
		r.PullRequestURL = &prURL
		stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}
		na := nextActionsFor(r, stages, nil, naReviewStatus("implement", "complete"), nil, nil,
			false, false, false, "", "", releaseSignals{})
		if na == nil || na.State != "succeeded_pr_open" {
			t.Fatalf("state = %+v, want succeeded_pr_open (graceful degradation)", na)
		}
	})

	// Partition control (the design question #2512 settles): not_validated and
	// undecidable are mutually exclusive BY CONSTRUCTION, and each must keep its
	// own label. A drift that collapsed one arm into the other would make a
	// zero-observation short-circuit and a ran-but-undecided stage read
	// identically to the operator, which is precisely what this change removes.
	t.Run("not_validated keeps its own distinct state", func(t *testing.T) {
		r := naLocalRun("running")
		r.PullRequestURL = &prURL
		na := nextActionsFor(r, naAcceptanceStages("succeeded"), nil,
			naReviewStatus("implement", "complete"), nil, nil, false, false, false,
			acceptanceVerdictNotValidated, "", releaseSignals{})
		if na == nil || na.State != "acceptance_not_validated" {
			t.Fatalf("state = %+v, want acceptance_not_validated — the two arms must not collapse", na)
		}
	})
}

// TestNextActions_SucceededAcceptanceSkippedOutOfScope pins the E38.3 (#1657)
// arm: a succeeded run with an open PR AND the acceptanceSkippedOutOfScope flag
// set classifies succeeded_acceptance_skipped_out_of_scope and STILL returns the
// rewired merge ritual (approve_pr -> fishhawk_merge_run) — the run stays
// merge-eligible. The graceful-degradation control proves the flag gates ONLY
// the label: with the flag false (the skip entry aged out of the recent window)
// the same run falls back to plain succeeded_pr_open, itself merge-eligible.
func TestNextActions_SucceededAcceptanceSkippedOutOfScope(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naRun("succeeded")
	run.PullRequestURL = &prURL
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}

	t.Run("flag set -> labeled state, still merge-eligible", func(t *testing.T) {
		na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, nil, false, true, false, "", "", releaseSignals{})
		if na == nil || na.State != "succeeded_acceptance_skipped_out_of_scope" {
			t.Fatalf("state = %+v, want succeeded_acceptance_skipped_out_of_scope", na)
		}
		if got := actionNames(na); len(got) != 2 || got[0] != "approve_pr" || got[1] != "fishhawk_merge_run" {
			t.Fatalf("actions = %v, want the rewired merge ritual [approve_pr fishhawk_merge_run]", got)
		}
	})

	t.Run("flag false (aged out) -> falls back to succeeded_pr_open", func(t *testing.T) {
		na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, nil, false, false, false, "", "", releaseSignals{})
		if na == nil || na.State != "succeeded_pr_open" {
			t.Fatalf("state = %+v, want succeeded_pr_open (graceful degradation)", na)
		}
		if got := actionNames(na); len(got) != 2 || got[0] != "approve_pr" {
			t.Fatalf("actions = %v, want the merge ritual", got)
		}
	})

	// mergeObserved wins over the skip flag: once the merge is observed the run
	// is succeeded_merged regardless of the acceptance-skip label.
	t.Run("mergeObserved wins", func(t *testing.T) {
		na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, nil, true, true, false, "", "", releaseSignals{})
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
	// The reason now names the relabel-and-re-poll remedy: the human_led
	// classification is re-read from the issue's autonomy:* label on each poll
	// (#2355), so an operator can drive the item agent-led by relabelling and
	// re-polling. Assert the discoverability wording is present.
	for _, want := range []string{"re-read", "autonomy:", "relabel", "re-poll"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("attend_human_led reason %q missing %q", got.Reason, want)
		}
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

// TestCampaignNextActionsFor_Closed pins the #2681 arm: a CAMPAIGN that went
// terminal with an issue still unfinished classifies as campaign_closed with
// exactly ONE action — a STANDALONE fishhawk_start_run on the stranded ref,
// never fishhawk_start_campaign_item_run (which the campaign gate would refuse).
func TestCampaignNextActionsFor_Closed(t *testing.T) {
	na := campaignNextActionsFor(
		CampaignRollup{Done: []string{"#26"}, Cancelled: []string{"#29"}},
		caNextAction("closed", "issue:29"))
	if na.State != "campaign_closed" {
		t.Fatalf("State = %q, want campaign_closed", na.State)
	}
	if len(na.Actions) != 1 {
		t.Fatalf("actions = %+v, want exactly 1", na.Actions)
	}
	got := na.Actions[0]
	if got.Action != "fishhawk_start_run" {
		t.Errorf("action = %q, want fishhawk_start_run (a campaign verb would be refused)", got.Action)
	}
	if got.Params["trigger_ref"] != "issue:29" {
		t.Errorf("params = %v, want trigger_ref issue:29", got.Params)
	}
	if got.Consumes != consumesNewRun {
		t.Errorf("consumes = %q, want %q", got.Consumes, consumesNewRun)
	}
	if !strings.Contains(got.Precondition, "terminal") {
		t.Errorf("precondition = %q, want it to name the terminal campaign", got.Precondition)
	}
	if !strings.Contains(got.Reason, "standalone") {
		t.Errorf("reason = %q, want it to say the issue must be driven standalone", got.Reason)
	}

	// A closed campaign with no stranded ref still carries the action (the
	// "never unclassified" structural invariant) with empty params.
	bare := campaignNextActionsFor(CampaignRollup{}, caNextAction("closed", ""))
	if bare.State != "campaign_closed" || len(bare.Actions) != 1 {
		t.Fatalf("refless closed = %+v, want campaign_closed with 1 action", bare)
	}
	if _, ok := bare.Actions[0].Params["trigger_ref"]; ok {
		t.Errorf("refless closed params = %v, want no trigger_ref", bare.Actions[0].Params)
	}
}

// TestCampaignNextActionsFor_UnknownAction_Unclassified pins the "never
// unclassified" invariant: a future backend-added action value lands in the
// labeled fallback with a NON-empty actions list — proving the classifier
// never returns an empty/unrouted result for a non-complete campaign. `closed`
// is asserted NOT to land here (it has its own arm as of #2681).
func TestCampaignNextActionsFor_UnknownAction_Unclassified(t *testing.T) {
	if got := campaignNextActionsFor(CampaignRollup{}, caNextAction("closed", "#99")); got.State == "campaign_unclassified" {
		t.Errorf("closed classified as %q, want its own campaign_closed arm", got.State)
	}
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
			wantActions: []string{"approve_pr", "fishhawk_merge_run"},
		},
		{
			// (4b, #2347) acceptance succeeded + verdict not_validated ->
			// acceptance_not_validated + the SAME merge ritual. Merge-eligible by
			// design: the change makes the outcome honest, not obstructive.
			name:        "4b_acceptance_not_validated_merge_ritual",
			run:         withPR(naLocalRun("running")),
			stages:      naAcceptanceStages("succeeded"),
			verdict:     "not_validated",
			wantState:   "acceptance_not_validated",
			wantActions: []string{"approve_pr", "fishhawk_merge_run"},
		},
		{
			// (4c, #2512) acceptance succeeded + verdict undecidable ->
			// acceptance_undecidable + the SAME merge ritual. Merge-eligible with
			// NO arbitration: an undecidable row is not a defect, so the run must
			// not be routed to the triage arm below.
			name:        "4c_acceptance_undecidable_merge_ritual",
			run:         withPR(naLocalRun("running")),
			stages:      naAcceptanceStages("succeeded"),
			verdict:     "undecidable",
			wantState:   "acceptance_undecidable",
			wantActions: []string{"approve_pr", "fishhawk_merge_run"},
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
			na := nextActionsFor(tc.run, tc.stages, nil, naReviewStatus("implement", "complete"), nil, nil, false, false, false, tc.verdict, tc.disposition, releaseSignals{})
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

// TestNextActions_AcceptanceFailedVerdict_NoTriageDisposition pins the arm for
// the state the #2512 severity ladder made newly REACHABLE: a recorded `failed`
// acceptance verdict with NO acceptance_triage_decided disposition.
//
// A body the agent ships as `passed` carrying a `failed` criterion row records
// `failed` (acceptanceVerdictAtLeast) and gates the merge at
// acceptanceGateTriage, but handleShipAcceptance keys triage on the AGENT's own
// failed claim, so no classifier runs and no disposition is written. Before
// #2512 every recorded failed verdict got a disposition, so this arm was
// unreachable and the empty-disposition case fell to the rerouting POLL — which
// would tell the operator to wait for a re-opened stage that never appears.
//
// The load-bearing assertion is that the arm surfaces
// fishhawk_arbitrate_acceptance: it is the only verb that discharges
// acceptanceGateTriage for this outcome, so without it the run's merge gate is
// undischargeable in-loop.
func TestNextActions_AcceptanceFailedVerdict_NoTriageDisposition(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naLocalRun("running")
	run.PullRequestURL = &prURL

	na := nextActionsFor(run, naAcceptanceStages("succeeded"), nil,
		naReviewStatus("implement", "complete"), nil, nil, false, false, false,
		acceptanceVerdictFailed, "", releaseSignals{})
	if na == nil {
		t.Fatal("nextActionsFor returned nil")
	}
	if na.State != "acceptance_triage_no_disposition" {
		t.Fatalf("state = %q, want acceptance_triage_no_disposition (NOT the rerouting poll arm)", na.State)
	}
	got := actionNames(na)
	want := []string{
		"fishhawk_list_audit",
		"fishhawk_fixup_stage",
		"fishhawk_arbitrate_acceptance",
		"merge_and_file_follow_up",
		"fishhawk_cancel_run",
		"fishhawk_get_run_status",
	}
	if len(got) != len(want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("actions[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	// The leading audit read must describe the NO-disposition case, not the
	// paged-disposition case it is derived from — an operator reading a
	// precondition that claims a paged disposition landed would go looking for
	// an acceptance_triage_decided entry that does not exist.
	if strings.Contains(firstPrecondition(na), "landed on a paged triage disposition") {
		t.Error("leading audit-read precondition still claims a paged disposition landed")
	}
	if !strings.Contains(firstPrecondition(na), "NO acceptance_triage_decided disposition") {
		t.Errorf("leading audit-read precondition does not name the no-disposition case: %q", firstPrecondition(na))
	}
	// Every action still carries the structured fields.
	for i, a := range na.Actions {
		if a.Precondition == "" || a.Consumes == "" || a.Reason == "" {
			t.Errorf("actions[%d] (%s) missing precondition/consumes/reason: %+v", i, a.Action, a)
		}
	}

	// The rewrite must not leak into the PAGED arm, which shares the same
	// underlying action list — acceptanceTriagePagedActions returns a fresh
	// slice per call, so mutating element 0 here must leave the paged arm's own
	// precondition intact.
	pagedNA := nextActionsFor(run, naAcceptanceStages("succeeded"), nil,
		naReviewStatus("implement", "complete"), nil, nil, false, false, false,
		acceptanceVerdictFailed, acceptanceDispositionPaged, releaseSignals{})
	if pagedNA == nil || !strings.Contains(firstPrecondition(pagedNA), "landed on a paged triage disposition") {
		t.Errorf("paged arm's leading precondition was clobbered by the no-disposition rewrite: %+v", pagedNA)
	}
}

// firstPrecondition returns the first suggested action's precondition.
func firstPrecondition(na *NextActions) string {
	if na == nil || len(na.Actions) == 0 {
		return ""
	}
	return na.Actions[0].Precondition
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
				naReviewStatus("implement", "complete"), nil, nil, false, false, false, "failed", disp, releaseSignals{})
			if na == nil || na.State != "acceptance_triage_paged" {
				t.Fatalf("state = %+v, want acceptance_triage_paged (not acceptance_triage_rerouting) for disposition %q", na, disp)
			}
			got := actionNames(na)
			// fishhawk_arbitrate_acceptance (E66.37 / #2474) sits between the
			// fix-up route and merge_and_file_follow_up: it is the audited
			// discharge that makes the merge legal, so it must be offered BEFORE
			// the merge step it unblocks.
			want := []string{"fishhawk_list_audit", "fishhawk_fixup_stage", "fishhawk_arbitrate_acceptance", "merge_and_file_follow_up", "fishhawk_cancel_run"}
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
				naReviewStatus("implement", "complete"), nil, nil, false, false, false, tc.verdict, "", releaseSignals{})
			if na == nil || na.State != "acceptance_settled_outcome_unknown" {
				t.Fatalf("state = %+v, want acceptance_settled_outcome_unknown", na)
			}
			if na.Actions[0].Action != "fishhawk_list_audit" {
				t.Errorf("actions[0] = %q, want fishhawk_list_audit", na.Actions[0].Action)
			}
			var retry *SuggestedAction
			for i := range na.Actions {
				a := na.Actions[i]
				if a.Action == "approve_pr" || a.Action == "merge_pr" || a.Action == "fishhawk_merge_run" {
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
			naReviewStatus("implement", "complete"), nil, nil, false, true, false, "", "", releaseSignals{})
		if na == nil || na.State != "acceptance_skipped_out_of_scope" {
			t.Fatalf("state = %+v, want acceptance_skipped_out_of_scope", na)
		}
		if got := actionNames(na); len(got) != 2 || got[0] != "approve_pr" || got[1] != "fishhawk_merge_run" {
			t.Fatalf("actions = %v, want the rewired merge ritual [approve_pr fishhawk_merge_run]", got)
		}
		for _, a := range na.Actions {
			if a.Action == "fishhawk_retry_stage" {
				t.Errorf("fishhawk_retry_stage must NOT be offered for the skip disposition (server 422s it): %+v", na.Actions)
			}
		}
	})

	t.Run("flag false (aged out) -> read-first outcome-unknown arm", func(t *testing.T) {
		na := nextActionsFor(newRun(), naAcceptanceStages("succeeded"), nil,
			naReviewStatus("implement", "complete"), nil, nil, false, false, false, "", "", releaseSignals{})
		if na == nil || na.State != "acceptance_settled_outcome_unknown" {
			t.Fatalf("state = %+v, want acceptance_settled_outcome_unknown (graceful degradation)", na)
		}
		if na.Actions[0].Action != "fishhawk_list_audit" {
			t.Errorf("actions[0] = %q, want fishhawk_list_audit", na.Actions[0].Action)
		}
		for _, a := range na.Actions {
			if a.Action == "approve_pr" || a.Action == "merge_pr" || a.Action == "fishhawk_merge_run" {
				t.Errorf("merge ritual must not surface on the aged-out arm: %+v", na.Actions)
			}
		}
	})

	t.Run("failed verdict + flag set -> triage wins", func(t *testing.T) {
		na := nextActionsFor(newRun(), naAcceptanceStages("succeeded"), nil,
			naReviewStatus("implement", "complete"), nil, nil, false, true, false, "failed", "paged", releaseSignals{})
		if na == nil || na.State != "acceptance_triage_paged" {
			t.Fatalf("state = %+v, want acceptance_triage_paged (recorded verdict wins over the flag)", na)
		}
	})
}

// TestNextActions_NoAcceptanceStage_MergeRitualUnchanged covers mode (9): a run
// with no acceptance stage keeps the implement_gate_settled state and the
// rewired approve_pr -> fishhawk_merge_run merge ritual (E48.7 / #1954).
func TestNextActions_NoAcceptanceStage_MergeRitualUnchanged(t *testing.T) {
	run := naRun("running")
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}
	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, nil, false, false, false, "", "", releaseSignals{})
	if na == nil || na.State != "implement_gate_settled" {
		t.Fatalf("state = %+v, want implement_gate_settled", na)
	}
	if got := actionNames(na); len(got) != 2 || got[0] != "approve_pr" || got[1] != "fishhawk_merge_run" {
		t.Fatalf("actions = %v, want [approve_pr fishhawk_merge_run]", got)
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
		"acceptanceVerdictNotValidated":     acceptanceVerdictNotValidated,
		"acceptanceVerdictUndecidable":      acceptanceVerdictUndecidable,
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
		// #2347: the SERVER-INTERNAL third verdict. Pinned here because the MCP
		// mirrors rather than imports it (#875) — a rename on the plan/server side
		// with no mirror silently routes every short-circuited run into the
		// defensive acceptance_settled_outcome_unknown arm.
		"acceptanceVerdictNotValidated": "not_validated",
		// #2512: the SERVER-DERIVED fourth verdict, mirrored for the same reason
		// and carrying the same failure mode — a rename with no mirror routes
		// every undecidable run into the defensive
		// acceptance_settled_outcome_unknown arm, which offers a retry that will
		// only reproduce the same undecidable rows.
		"acceptanceVerdictUndecidable":   "undecidable",
		"fixup_dispatched":               "fixup_dispatched",
		"retry_dispatched":               "retry_dispatched",
		"paged":                          "paged",
		"rerun_budget_exhausted":         "rerun_budget_exhausted",
		"fixup_unavailable_paged":        "fixup_unavailable_paged",
		"retry_unavailable_paged":        "retry_unavailable_paged",
		"unsettled_paged":                "unsettled_paged",
		"externally_unvalidatable_paged": "externally_unvalidatable_paged",
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
	if got := acceptanceStageNextActions(run, acc("pending"), false, false, "", "").State; got != string(drive.RuleAcceptancePending) {
		t.Errorf("pending state = %q, want %q (drive.RuleAcceptancePending)", got, drive.RuleAcceptancePending)
	}
	// settled-outcome-unknown: a terminal acceptance stage with no verdict.
	if got := acceptanceStageNextActions(run, acc("succeeded"), false, false, "", "").State; got != string(drive.RuleAcceptanceOutcomeUnknown) {
		t.Errorf("outcome-unknown state = %q, want %q (drive.RuleAcceptanceOutcomeUnknown)", got, drive.RuleAcceptanceOutcomeUnknown)
	}
	// skipped-out-of-scope (E38.3 / #1877): a terminal verdict-less acceptance
	// stage WITH the skip flag classifies acceptance_skipped_out_of_scope — the
	// MCP state string MUST equal the audit-category marker so the drive gate
	// state (server acceptanceGateSkippedOutOfScope) and this surface agree.
	if got := acceptanceStageNextActions(run, acc("succeeded"), true, false, "", "").State; got != auditCategoryAcceptanceSkippedOutOfScope {
		t.Errorf("skip state = %q, want %q (audit-category marker)", got, auditCategoryAcceptanceSkippedOutOfScope)
	}
	// failed arm: paged + rerouting states both carry the drive triage prefix.
	paged := acceptanceStageNextActions(run, acc("succeeded"), false, false, acceptanceVerdictFailed, acceptanceDispositionPaged).State
	rerouting := acceptanceStageNextActions(run, acc("succeeded"), false, false, acceptanceVerdictFailed, acceptanceDispositionFixupDispatched).State
	// #2512: the empty-disposition arm is a THIRD failed-arm state, so it must
	// carry the same drive triage prefix.
	noDisposition := acceptanceStageNextActions(run, acc("succeeded"), false, false, acceptanceVerdictFailed, "").State
	for _, st := range []string{paged, rerouting, noDisposition} {
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
			na := nextActionsFor(run, nil, nil, nil, nil, nil, false, false, false, "", "", tc.sig)
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
		na := nextActionsFor(naReleaseRun(), nil, nil, nil, nil, nil, false, false, false, "", "",
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
		na := nextActionsFor(naReleaseRun(), nil, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
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

// quotaSeamRepo is a minimal runmodel.Repository sufficient to walk the
// stage-failure-reason PERSIST -> READ seam end to end: runmodel.FailStage
// (persist) uses GetStage + TransitionStage, the backend's GET
// /v0/runs/{id}/stages read handler uses GetRun (via requireRunAccount) +
// ListStagesForRun. Every other method is promoted from the embedded nil
// interface and is never called on this path. It is deliberately NOT a
// StageCASTransitioner, so runmodel.FailStage takes its non-CAS branch.
type quotaSeamRepo struct {
	runmodel.Repository
	mu     sync.Mutex
	runRow *runmodel.Run
	stages map[uuid.UUID]*runmodel.Stage
}

func (r *quotaSeamRepo) GetRun(_ context.Context, id uuid.UUID) (*runmodel.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runRow != nil && r.runRow.ID == id {
		cp := *r.runRow
		return &cp, nil
	}
	return nil, runmodel.ErrNotFound
}

func (r *quotaSeamRepo) GetStage(_ context.Context, id uuid.UUID) (*runmodel.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stages[id]
	if !ok {
		return nil, runmodel.ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (r *quotaSeamRepo) TransitionStage(_ context.Context, id uuid.UUID, to runmodel.StageState, c *runmodel.StageCompletion) (*runmodel.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stages[id]
	if !ok {
		return nil, runmodel.ErrNotFound
	}
	s.State = to
	if c != nil {
		s.FailureCategory = c.FailureCategory
		s.FailureReason = c.FailureReason
	}
	cp := *s
	return &cp, nil
}

func (r *quotaSeamRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*runmodel.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*runmodel.Stage
	for _, s := range r.stages {
		if s.RunID == runID {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out, nil
}

// TestNextActions_QuotaReasonSurvivesPersistAndRead is the transit-coverage
// integration test for the #2085 quota classification (binding operator
// condition 1). It walks the BACKEND half of the runner->operator seam end to
// end: a realistic-length quota FailureReason is PERSISTED via the exact call
// the runner's trace-upload path makes (runmodel.FailStage with FailureA), then READ
// back through the real backend GET /v0/runs/{id}/stages HTTP handler
// (ListStagesForRun -> toStageResponse -> JSON) by the real MCP client, and fed
// to implementFailedNextActions. The classifier must yield the quota-aware
// guidance ("wait for the cap to reset", "not a transient crash") with the
// phrase INTACT — so any truncation/transform of FailureReason in persistence
// or read (the two go.work modules can't share the literal, the #1548
// limitation) fails this test rather than silently degrading the operator hint.
func TestNextActions_QuotaReasonSurvivesPersistAndRead(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	repo := &quotaSeamRepo{
		// Untenanted run (AccountID ""), so the readAccess account gate admits
		// the anonymous identity the test's tokenless client carries.
		runRow: &runmodel.Run{ID: runID, Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", State: runmodel.StateFailed},
		stages: map[uuid.UUID]*runmodel.Stage{
			stageID: {
				ID:           stageID,
				RunID:        runID,
				Sequence:     1,
				Type:         runmodel.StageTypeImplement,
				ExecutorKind: runmodel.ExecutorAgent,
				ExecutorRef:  "claude-code",
				State:        runmodel.StageStateRunning, // failable in one step
			},
		},
	}

	// The verbatim, realistic-length reason the runner's claudecode adapter
	// emits for a model-quota exhaustion. Persisted via the actual FailStage
	// call the trace-upload handler uses.
	const quotaReason = "could not obtain model quota (likely a usage/rate cap): agent exited with exit status 1 after 2s having made no model call (0 tokens)"
	if _, err := runmodel.FailStage(context.Background(), repo, stageID, runmodel.FailureA, quotaReason); err != nil {
		t.Fatalf("FailStage (persist): %v", err)
	}

	// Serve the real backend stage-read handler over HTTP.
	srv := server.New(server.Config{RunRepo: repo})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Read the persisted stage back through the real MCP client.
	client := newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"})
	stages, err := client.ListRunStages(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListRunStages (read): %v", err)
	}
	var impl *Stage
	for i := range stages {
		if stages[i].Type == "implement" {
			impl = &stages[i]
			break
		}
	}
	if impl == nil {
		t.Fatalf("no implement stage read back; got %+v", stages)
	}
	if impl.FailureCategory == nil || *impl.FailureCategory != "A" {
		t.Fatalf("FailureCategory = %v, want A", impl.FailureCategory)
	}
	// The phrase must survive persistence + read byte-for-byte.
	if impl.FailureReason == nil || *impl.FailureReason != quotaReason {
		t.Fatalf("FailureReason did not survive persist->read intact:\n got  %v\n want %q", impl.FailureReason, quotaReason)
	}

	// Feed the read-back stage to the classifier and assert the quota guidance.
	na := implementFailedNextActions(naRun("failed"), nil, nil, impl)
	retry := findAction(t, na, "fishhawk_retry_stage")
	if !strings.Contains(retry.Reason, "wait for the cap to reset") {
		t.Errorf("retry reason should tell the operator to wait for the cap to reset; got %q", retry.Reason)
	}
	if !strings.Contains(retry.Reason, "not a transient crash") {
		t.Errorf("retry reason should distinguish quota from a transient crash; got %q", retry.Reason)
	}
}

// TestNextActions_LiveValidationGuidance (#2045, E48.35) pins the operator
// live-validation guidance the classifier folds in from a decoded
// Run.LiveValidation block. Three rendered variants (binding condition A(1)):
// a healthy linked walk ("walk: #X"), a filing failure ("walk filing failed"),
// and a stranded intent-only marker ("walk filing incomplete" — condition
// A(3)). A run with no pending walk folds nothing in, and the pure renderer's
// degenerate inputs (nil / zero count / empty-ref-yet-healthy) are pinned
// directly.
func TestNextActions_LiveValidationGuidance(t *testing.T) {
	// The advisory folds in regardless of run state; use a settled succeeded run
	// with an open PR so the classifier also produces the ordinary merge ritual —
	// proving the advisory is APPENDED, never a replacement.
	prURL := "https://github.com/x/y/pull/42"
	baseRun := func(lv *RunLiveValidation) *Run {
		r := naRun("succeeded")
		r.PullRequestURL = &prURL
		r.LiveValidation = lv
		return r
	}
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}
	implComplete := naReviewStatus("implement", "complete")

	cases := []struct {
		name string
		lv   *RunLiveValidation
		want string
	}{
		{
			name: "healthy linked walk",
			lv:   &RunLiveValidation{PendingCriteriaCount: 3, WalkRef: "#123", FilingFailed: false},
			want: "3 criteria pending operator live-validation (walk: #123)",
		},
		{
			name: "filing failure",
			lv:   &RunLiveValidation{PendingCriteriaCount: 2, FilingFailed: true},
			want: "2 criteria pending operator live-validation (walk filing failed — file manually)",
		},
		{
			name: "stranded intent-only marker",
			lv:   &RunLiveValidation{PendingCriteriaCount: 1, FilingFailed: true, FilingIncomplete: true},
			want: "1 criteria pending operator live-validation (walk filing incomplete — file manually)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			na := nextActionsFor(baseRun(tc.lv), stages, nil, implComplete, nil, nil, false, false, false, "", "", releaseSignals{})
			act := findAction(t, na, "operator_live_validation")
			if !strings.Contains(act.Reason, tc.want) {
				t.Errorf("operator_live_validation reason = %q, want it to contain %q", act.Reason, tc.want)
			}
			if act.Consumes != consumesNone {
				t.Errorf("operator_live_validation consumes = %q, want none (display-only advisory)", act.Consumes)
			}
			// The advisory is APPENDED — the merge ritual is still present and
			// still leads (fishhawk_merge_run is not displaced by the advisory).
			names := actionNames(na)
			if !strings.Contains(strings.Join(names, ","), "fishhawk_merge_run") {
				t.Errorf("actions = %v, want the merge ritual retained alongside the advisory", names)
			}
			if names[len(names)-1] != "operator_live_validation" {
				t.Errorf("operator_live_validation should be appended LAST; actions = %v", names)
			}
		})
	}

	t.Run("healthy walk_ref reaches the action params", func(t *testing.T) {
		na := nextActionsFor(baseRun(&RunLiveValidation{PendingCriteriaCount: 3, WalkRef: "#123"}), stages, nil, implComplete, nil, nil, false, false, false, "", "", releaseSignals{})
		act := findAction(t, na, "operator_live_validation")
		if act.Params["walk_ref"] != "#123" {
			t.Errorf("params[walk_ref] = %q, want #123", act.Params["walk_ref"])
		}
	})

	t.Run("no pending walk folds nothing in", func(t *testing.T) {
		na := nextActionsFor(baseRun(nil), stages, nil, implComplete, nil, nil, false, false, false, "", "", releaseSignals{})
		for _, name := range actionNames(na) {
			if name == "operator_live_validation" {
				t.Fatalf("operator_live_validation must not appear when the run carries no live_validation block; actions = %v", actionNames(na))
			}
		}
	})

	// The pure renderer's degenerate inputs. A nil block, a zero pending count,
	// and a "healthy-yet-empty-ref" block (which the backend never writes) all
	// avoid a malformed "walk: " string — the empty-ref case degrades to the
	// file-manually wording (condition A(1): never a malformed empty-ref string).
	t.Run("renderer edge cases", func(t *testing.T) {
		if g := liveValidationGuidance(nil); g != "" {
			t.Errorf("nil block guidance = %q, want empty", g)
		}
		if g := liveValidationGuidance(&RunLiveValidation{PendingCriteriaCount: 0, WalkRef: "#1"}); g != "" {
			t.Errorf("zero-count guidance = %q, want empty", g)
		}
		g := liveValidationGuidance(&RunLiveValidation{PendingCriteriaCount: 2, FilingFailed: false, WalkRef: ""})
		if strings.Contains(g, "walk: ") {
			t.Errorf("empty-ref healthy block rendered a malformed walk-ref string: %q", g)
		}
		if !strings.Contains(g, "file manually") {
			t.Errorf("empty-ref healthy block should degrade to the file-manually wording; got %q", g)
		}
		// Defensive nil-na guard: never reached from nextActionsFor (na is always
		// non-nil at the fold call sites), but the guard must not panic if a
		// future caller passes nil.
		foldLiveValidationAdvisory(baseRun(&RunLiveValidation{PendingCriteriaCount: 1, WalkRef: "#1"}), nil)
	})
}

// TestNextActions_CarryBoundWorkingDir pins the foldWorkingDirParams fold
// (E66.42 / #2482): every emitted action whose verb is one of the four
// runner-spawning verbs carries params["working_dir"] equal to the run's
// binding, across the plan-approved dispatch arm and the run_children arm; a
// paired unbound-run case asserts the key is ABSENT (not empty), so the fold
// cannot pass by unconditionally writing a key. It also asserts the fold is
// SELECTIVE — a non-four-verb action (the poll) is not stamped.
func TestNextActions_CarryBoundWorkingDir(t *testing.T) {
	const bound = "/Users/dev/src/fishhawk"

	// Plan-approved dispatch arm: local run, plan succeeded + implement pending
	// → fishhawk_dispatch_stage + fishhawk_run_stage, both four-verb.
	t.Run("dispatch_arm_stamped", func(t *testing.T) {
		run := naRun("running")
		run.RunnerKind = "local"
		run.WorkingDir = bound
		na := nextActionsFor(run, []Stage{naStage("plan", "succeeded"), naStage("implement", "pending")},
			nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})

		for _, verb := range []string{"fishhawk_dispatch_stage", "fishhawk_run_stage"} {
			a := findAction(t, na, verb)
			if a.Params["working_dir"] != bound {
				t.Errorf("%s params[working_dir] = %q, want %q", verb, a.Params["working_dir"], bound)
			}
		}
		// Selective: the poll action (not a runner-spawning verb) is NOT stamped.
		for _, a := range na.Actions {
			if _, isFourVerb := workingDirInheritingActions[a.Action]; !isFourVerb {
				if _, has := a.Params["working_dir"]; has {
					t.Errorf("non-runner-spawning action %q should NOT carry working_dir; got %v", a.Action, a.Params)
				}
			}
		}
	})

	// run_children arm: awaiting_children → fishhawk_run_children.
	t.Run("run_children_arm_stamped", func(t *testing.T) {
		run := naRun("running")
		run.RunnerKind = "local"
		run.WorkingDir = bound
		na := nextActionsFor(run, []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_children")},
			nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
		a := findAction(t, na, "fishhawk_run_children")
		if a.Params["working_dir"] != bound {
			t.Errorf("fishhawk_run_children params[working_dir] = %q, want %q", a.Params["working_dir"], bound)
		}
	})

	// Unbound-run pairing: the key must be ABSENT (not an empty-string path the
	// agent would pass verbatim and get refused over HTTP).
	t.Run("unbound_run_key_absent", func(t *testing.T) {
		run := naRun("running")
		run.RunnerKind = "local"
		// WorkingDir intentionally empty (unbound).
		na := nextActionsFor(run, []Stage{naStage("plan", "succeeded"), naStage("implement", "pending")},
			nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
		a := findAction(t, na, "fishhawk_dispatch_stage")
		if _, has := a.Params["working_dir"]; has {
			t.Errorf("unbound run must NOT carry working_dir; got %v", a.Params)
		}
	})
}

// --- acceptance arbitration correlation (E66.37 / #2474) --------------------

// naOutcomeEntry / naArbitrationEntry build the two audit shapes
// acceptanceArbitratedIn correlates. Sequence is load-bearing: the correlation
// is payload outcome_sequence EQUALITY against the newest verdict's SEQUENCE,
// never slice position.
func naOutcomeEntry(seq int64, verdict string) AuditEntry {
	return AuditEntry{
		Category: auditCategoryAcceptanceOutcomeRecorded,
		Sequence: seq,
		Payload:  map[string]any{"verdict": verdict},
	}
}

func naArbitrationEntry(seq, outcomeSequence int64) AuditEntry {
	return AuditEntry{
		Category: auditCategoryAcceptanceTriageArbitrated,
		Sequence: seq,
		Payload: map[string]any{
			"reason": "operator discharge",
			acceptanceArbitrationOutcomeSequenceField: float64(outcomeSequence),
		},
	}
}

// TestAcceptanceArbitratedIn_MatchesNewestOutcome: the positive case — an
// arbitration naming the newest verdict's sequence discharges it.
func TestAcceptanceArbitratedIn_MatchesNewestOutcome(t *testing.T) {
	recent := []AuditEntry{
		naArbitrationEntry(11, 10),
		naOutcomeEntry(10, acceptanceVerdictFailed),
	}
	if !acceptanceArbitratedIn(recent) {
		t.Error("acceptanceArbitratedIn = false, want true when the arbitration names the newest outcome")
	}
}

// TestAcceptanceArbitratedIn_AppendedAfterNewerVerdictIgnored is BINDING
// APPROVAL CONDITION 4(b) on the MCP surface. The fixture is the case where the
// two candidate correlation rules DISAGREE:
//
//	outcome@10 failed, outcome@30 failed, arbitration@31 naming 10.
//
// An ORDERING rule ("an arbitration newer than the newest verdict correlates" —
// the idiom latestAcceptanceTriageDisposition uses) would say TRUE, because
// entry 31 is newer than verdict 30. The payload-EQUALITY rule the authoritative
// server gate applies says FALSE, because 10 != 30. This surface must agree with
// the server, or next_actions would offer a merge fishhawk_merge_run then 409s —
// the self-contradiction #2474 documents as its second-worst symptom.
//
// The SERVER twin over the byte-identical fixture is
// TestAcceptanceGateState_ArbitrationAppendedAfterNewerVerdictIgnored
// (backend/internal/server/acceptance_test.go). The two live in different
// packages by construction — this package must not import
// backend/internal/server (the #875 compile trap) and the server gate's
// classifier is unexported — so a single Go test cannot call both; the fixture
// and expectation are therefore mirrored, and each test names the other.
func TestAcceptanceArbitratedIn_AppendedAfterNewerVerdictIgnored(t *testing.T) {
	recent := []AuditEntry{
		naArbitrationEntry(31, 10), // NEWEST entry, but names the OLD outcome
		naOutcomeEntry(30, acceptanceVerdictFailed),
		naOutcomeEntry(10, acceptanceVerdictFailed),
	}
	if acceptanceArbitratedIn(recent) {
		t.Error("acceptanceArbitratedIn = true — correlation must be payload outcome_sequence EQUALITY, never ordering")
	}

	// The classifier consuming it must therefore stay in the paged-arbitration
	// arm and offer the arbitration verb, not the merge ritual.
	run := naLocalRun("running")
	prURL := "https://github.com/x/y/pull/42"
	run.PullRequestURL = &prURL
	na := nextActionsFor(run, naAcceptanceStages("succeeded"), nil,
		naReviewStatus("implement", "complete"), nil, nil, false, false,
		acceptanceArbitratedIn(recent), "failed", "externally_unvalidatable_paged", releaseSignals{})
	if na == nil || na.State != "acceptance_triage_paged" {
		t.Fatalf("state = %+v, want acceptance_triage_paged on a stale arbitration", na)
	}
	findAction(t, na, "fishhawk_arbitrate_acceptance")
}

// TestAcceptanceArbitratedIn_SupersededByNewerVerdictIgnored: the ordinary
// invalidation — an arbitration appended BEFORE the re-run's verdict.
func TestAcceptanceArbitratedIn_SupersededByNewerVerdictIgnored(t *testing.T) {
	recent := []AuditEntry{
		naOutcomeEntry(30, acceptanceVerdictFailed), // the re-run
		naArbitrationEntry(11, 10),
		naOutcomeEntry(10, acceptanceVerdictFailed),
	}
	if acceptanceArbitratedIn(recent) {
		t.Error("acceptanceArbitratedIn = true on a superseded arbitration, want false")
	}
}

// TestAcceptanceArbitratedIn_MalformedAndAbsent: every degrade returns false —
// the safe direction, since a false negative lands the operator in the arm that
// offers the arbitration verb rather than a merge the gate refuses.
func TestAcceptanceArbitratedIn_MalformedAndAbsent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		recent []AuditEntry
	}{
		{"nil slice", nil},
		{"no outcome entry", []AuditEntry{naArbitrationEntry(11, 10)}},
		{"no arbitration entry", []AuditEntry{naOutcomeEntry(10, acceptanceVerdictFailed)}},
		{"non-object payload", []AuditEntry{
			{Category: auditCategoryAcceptanceTriageArbitrated, Sequence: 11, Payload: "not-an-object"},
			naOutcomeEntry(10, acceptanceVerdictFailed),
		}},
		{"missing outcome_sequence", []AuditEntry{
			{Category: auditCategoryAcceptanceTriageArbitrated, Sequence: 11, Payload: map[string]any{"reason": "x"}},
			naOutcomeEntry(10, acceptanceVerdictFailed),
		}},
		{"non-numeric outcome_sequence", []AuditEntry{
			{Category: auditCategoryAcceptanceTriageArbitrated, Sequence: 11,
				Payload: map[string]any{acceptanceArbitrationOutcomeSequenceField: "10"}},
			naOutcomeEntry(10, acceptanceVerdictFailed),
		}},
		{"non-integral outcome_sequence", []AuditEntry{
			{Category: auditCategoryAcceptanceTriageArbitrated, Sequence: 11,
				Payload: map[string]any{acceptanceArbitrationOutcomeSequenceField: 10.5}},
			naOutcomeEntry(10, acceptanceVerdictFailed),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if acceptanceArbitratedIn(tc.recent) {
				t.Error("acceptanceArbitratedIn = true, want false")
			}
		})
	}
}

// TestAcceptanceArbitrationCategoryLiteral pins the cross-module literal seam:
// the backend WRITES this category string and this classifier READS it. A
// backend rename not mirrored here would silently stop discharging every
// arbitration, wedging the operator right back where #2474 found them.
func TestAcceptanceArbitrationCategoryLiteral(t *testing.T) {
	if auditCategoryAcceptanceTriageArbitrated != "acceptance_triage_arbitrated" {
		t.Errorf("auditCategoryAcceptanceTriageArbitrated = %q, want acceptance_triage_arbitrated",
			auditCategoryAcceptanceTriageArbitrated)
	}
	if acceptanceArbitrationOutcomeSequenceField != "outcome_sequence" {
		t.Errorf("acceptanceArbitrationOutcomeSequenceField = %q, want outcome_sequence",
			acceptanceArbitrationOutcomeSequenceField)
	}
}

// TestNextActions_AcceptanceArbitrated_OffersMergeRitual: a failed verdict whose
// paged triage an operator discharged surfaces the merge ritual under its own
// acceptance_arbitrated state, checked BEFORE the paged branch so a discharged
// run is never re-offered the arbitration it already has.
func TestNextActions_AcceptanceArbitrated_OffersMergeRitual(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naLocalRun("running")
	run.PullRequestURL = &prURL

	na := nextActionsFor(run, naAcceptanceStages("succeeded"), nil,
		naReviewStatus("implement", "complete"), nil, nil, false, false, true,
		"failed", "externally_unvalidatable_paged", releaseSignals{})
	if na == nil || na.State != "acceptance_arbitrated" {
		t.Fatalf("state = %+v, want acceptance_arbitrated", na)
	}
	findAction(t, na, "fishhawk_merge_run")
	for _, a := range na.Actions {
		if a.Action == "fishhawk_arbitrate_acceptance" {
			t.Error("an already-arbitrated run must not be offered the arbitration verb again")
		}
	}
	// The arm's prose must keep the override honest — an arbitrated failure is
	// merge-eligible but is NOT a validated pass, and the operator is asked to
	// say so in their merge verdict. mergeRitualActions threads the arm's `why`
	// onto the approve_pr entry, so that is where the claim lands.
	approve := findAction(t, na, "approve_pr")
	if !strings.Contains(strings.ToLower(approve.Reason), "not a validated pass") {
		t.Errorf("the arbitrated arm must state it is NOT a validated pass; got %q", approve.Reason)
	}
	if !strings.Contains(approve.Reason, "acceptance_triage_arbitrated") {
		t.Errorf("the arbitrated arm must name the discharge that admitted the merge; got %q", approve.Reason)
	}
}

// TestNextActions_AcceptanceArbitratedFlagIgnoredOnNonFailedVerdict: the flag is
// scoped to the FAILED arm. A passed verdict keeps acceptance_passed even if a
// stale arbitration flag is somehow true, so the flag can never relabel a
// genuine pass as an override.
func TestNextActions_AcceptanceArbitratedFlagIgnoredOnNonFailedVerdict(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	run := naLocalRun("running")
	run.PullRequestURL = &prURL

	na := nextActionsFor(run, naAcceptanceStages("succeeded"), nil,
		naReviewStatus("implement", "complete"), nil, nil, false, false, true,
		"passed", "", releaseSignals{})
	if na == nil || na.State != "acceptance_passed" {
		t.Fatalf("state = %+v, want acceptance_passed (the arbitration flag is scoped to the failed arm)", na)
	}
}

// TestNextActions_PollCadenceMatchesWaitStatus pins the E48.62 / #2489 parity
// requirement: the poll_interval_seconds a pollAction advertises must be the
// SAME derived cadence the corresponding *_stage_wait_status carries. Shipping
// a flat 30s in next_actions while the wait status says 375 would be a visible
// contradiction on one snapshot.
//
// The derived value is read against time.Now() inside the arm, so the
// assertion compares next_actions against stageWaitStatusFor computed on the
// same inputs (tolerating a one-second boundary crossing) rather than against a
// hardcoded literal.
func TestNextActions_PollCadenceMatchesWaitStatus(t *testing.T) {
	started := time.Now().UTC().Add(-5400 * time.Second)
	run := naRun("running")
	run.PredictedRuntimeMinutes = 115

	impl := naStage("implement", "running")
	impl.StartedAt = &started
	stages := []Stage{naStage("plan", "succeeded"), impl}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	if na == nil || na.State != "implement_running" {
		t.Fatalf("next actions = %+v, want state implement_running", na)
	}
	var advertised string
	for _, a := range na.Actions {
		if a.Action == "fishhawk_get_run_status" {
			advertised = a.Params["poll_interval_seconds"]
		}
	}
	if advertised == "" {
		t.Fatal("no fishhawk_get_run_status poll action found")
	}
	got, err := strconv.Atoi(advertised)
	if err != nil {
		t.Fatalf("poll_interval_seconds %q is not an integer: %v", advertised, err)
	}
	// (115*60 - 5400) / 4 = 375. The floor (30) is what a non-derived arm would
	// advertise, so the band is deliberately far too narrow to admit it.
	if got < 374 || got > 375 {
		t.Errorf("next_actions poll_interval_seconds = %d, want 375 (the derived cadence, not the flat floor)", got)
	}

	// And it agrees with the wait status computed over the same snapshot.
	ws := stageWaitStatusFor(stages, "implement", run.State, run.PredictedRuntimeMinutes, time.Now().UTC())
	if ws == nil {
		t.Fatal("implement stage wait status is nil")
	}
	if diff := ws.PollIntervalSeconds - got; diff < -1 || diff > 1 {
		t.Errorf("next_actions poll (%d) disagrees with wait status poll (%d)", got, ws.PollIntervalSeconds)
	}
}

// TestNextActions_StagelessArmsKeepTheFloor pins the two arms that DELIBERATELY
// keep the bare floor (E48.62 / #2489): stages_pending (no stage row exists yet,
// so there is nothing to derive from) and awaiting_children (the parent's stage
// is parked; the live progress is on the child runs). Both are seeded under a
// run carrying a 115-minute prediction, so an arm that wrongly derived would
// advertise 900 here and fail.
func TestNextActions_StagelessArmsKeepTheFloor(t *testing.T) {
	pollOf := func(t *testing.T, na *NextActions) int {
		t.Helper()
		for _, a := range na.Actions {
			if a.Action == "fishhawk_get_run_status" {
				n, err := strconv.Atoi(a.Params["poll_interval_seconds"])
				if err != nil {
					t.Fatalf("poll_interval_seconds is not an integer: %v", err)
				}
				return n
			}
		}
		t.Fatal("no fishhawk_get_run_status poll action found")
		return 0
	}

	run := naRun("running")
	run.PredictedRuntimeMinutes = 115

	naEmpty := nextActionsFor(run, nil, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	if naEmpty == nil || naEmpty.State != "stages_pending" {
		t.Fatalf("no-stages next actions = %+v, want state stages_pending", naEmpty)
	}
	if got := pollOf(t, naEmpty); got != suggestedStageWaitPollIntervalSeconds {
		t.Errorf("stages_pending poll = %d, want %d (the floor)", got, suggestedStageWaitPollIntervalSeconds)
	}

	awaiting := []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_children")}
	naChildren := nextActionsFor(run, awaiting, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	if naChildren == nil || naChildren.State != "implement_awaiting_children" {
		t.Fatalf("awaiting-children next actions = %+v, want state implement_awaiting_children", naChildren)
	}
	if got := pollOf(t, naChildren); got != suggestedStageWaitPollIntervalSeconds {
		t.Errorf("awaiting_children poll = %d, want %d (the floor)", got, suggestedStageWaitPollIntervalSeconds)
	}
}

// TestNextActions_AwaitingScopeDecision_GeneralizedToBothShortfallClasses pins
// the #2501 generalization: the awaiting_scope_decision arm no longer names the
// #1151 missing-declared-scope-file check as the ONLY reason a stage parks (an
// unsatisfied binding assertion parks it too), and the reason states that
// exempt RETURNS THE STAGE TO DISPATCH rather than opening the PR instantly.
func TestNextActions_AwaitingScopeDecision_GeneralizedToBothShortfallClasses(t *testing.T) {
	run := naRun("running")
	// A park carrying ONLY unsatisfied assertions presents identically at this
	// layer (the state is the signal), so the same arm must still fire.
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_scope_decision")}
	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	if na == nil || na.State != "implement_awaiting_scope_decision" {
		t.Fatalf("state = %+v, want implement_awaiting_scope_decision for an assertion-class park too", na)
	}
	dec := findAction(t, na, "fishhawk_decide_scope_completeness")
	if !strings.Contains(dec.Precondition, "binding assertion") {
		t.Errorf("precondition must name the SECOND shortfall class; got %q", dec.Precondition)
	}
	if !strings.Contains(dec.Precondition, "unsatisfied_assertions") {
		t.Errorf("precondition must point the operator at the unsatisfied_assertions payload key; got %q", dec.Precondition)
	}
	if !strings.Contains(dec.Reason, "returns the stage to dispatch") {
		t.Errorf("reason must state that exempt returns the stage to dispatch (not that the PR opens instantly); got %q", dec.Reason)
	}
}

// TestNextActions_AwaitingScopeDecision_OffersAmend pins the #2591 surface: the
// parked arm must tell the operator amend EXISTS and that it is the only
// non-fail resolution for a build-required park — an operator who only ever
// sees "exempt|fail" on the class whose exempt is refused has no route forward
// but to fail the pass, which is the whole problem the amend decision closes.
func TestNextActions_AwaitingScopeDecision_OffersAmend(t *testing.T) {
	run := naRun("running")
	stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_scope_decision")}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	dec := findAction(t, na, "fishhawk_decide_scope_completeness")
	if !strings.Contains(dec.Params["decision"], "amend") {
		t.Errorf("decision hint must offer amend; got %q", dec.Params["decision"])
	}
	if !strings.Contains(dec.Reason, "amend widens") {
		t.Errorf("reason must describe what amend does; got %q", dec.Reason)
	}
	if !strings.Contains(dec.Reason, "build_required_paths") {
		t.Errorf("reason must name amend as the non-fail resolution for a build_required_paths park; got %q", dec.Reason)
	}
	if !strings.Contains(dec.Precondition, "build_required_paths") {
		t.Errorf("precondition must point the operator at the build_required_paths payload key; got %q", dec.Precondition)
	}
}

// --- failure-signature block (#1703) ---------------------------------------

// naFailedStageWithProgress builds a failed stage carrying a stage_progress
// heartbeat, so the counter-anchored signature can be driven.
func naFailedStageWithProgress(category, reason string, turns, tokens int) Stage {
	s := naFailedImplement(category, reason)
	s.Progress = &StageProgress{LastEvent: "assistant", TurnsThisAttempt: turns, TokensThisAttempt: tokens}
	return s
}

// TestNextActions_SignatureAttachedOnMatch is the CROSS-BOUNDARY test: a
// persistence-shaped failed Stage row travels failureSignatureFor's Evidence
// adapter, through failuresig.Match, into the rendered NextActions block. The
// fixture's reason is the literal the runner emits
// (runner/cmd/fishhawk-runner/main.go:779's runner_failed reason, relayed
// verbatim into failure_reason by the reap-failure endpoint), so a change in
// that relay breaks the fixture's realism.
//
// Per-layer units alone would pass while the adapter dropped a field; this
// pins the whole seam.
func TestNextActions_SignatureAttachedOnMatch(t *testing.T) {
	run := naRun("failed")
	stages := []Stage{
		naStage("plan", "succeeded"),
		naFailedImplement("C", `lineage_lock: another runner holds the lineage lock (detail: "run 8f3a")`),
	}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	if na == nil {
		t.Fatal("nextActionsFor returned nil")
	}
	if na.Signature == nil {
		t.Fatal("Signature is nil, want lineage_lock_contention")
	}
	if na.Signature.ID != "lineage_lock_contention" {
		t.Fatalf("Signature.ID = %q, want lineage_lock_contention", na.Signature.ID)
	}
	if na.Signature.RegistryVersion != failuresig.RegistryVersion {
		t.Errorf("Signature.RegistryVersion = %q, want %q", na.Signature.RegistryVersion, failuresig.RegistryVersion)
	}
	if len(na.Signature.Playbook) == 0 {
		t.Error("Signature.Playbook is empty — the hint carries no recovery")
	}
	if na.Signature.Means == "" || na.Signature.Title == "" {
		t.Errorf("Signature is incomplete: %+v", na.Signature)
	}
}

// TestNextActions_SignatureCounterAnchoredSeam pins the OTHER adapter arm: the
// counter-anchored signature reads stage.Progress and run.RetryAttempt, so
// this fails if the adapter drops either.
func TestNextActions_SignatureCounterAnchoredSeam(t *testing.T) {
	run := naRun("failed")
	run.RetryAttempt = 2
	stages := []Stage{
		naStage("plan", "succeeded"),
		naFailedStageWithProgress("A", "agent exited with exit status 1", 0, 0),
	}
	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	if na == nil || na.Signature == nil {
		t.Fatalf("Signature is nil, want agent_no_progress_repeat (na=%+v)", na)
	}
	if na.Signature.ID != "agent_no_progress_repeat" {
		t.Fatalf("Signature.ID = %q, want agent_no_progress_repeat", na.Signature.ID)
	}
}

// naGenericCategoryAReason is the classifier's generic category-A retry reason
// — the exact prose an unrecognized category-A failure has always produced.
// Asserting the LITERAL (rather than diffing two calls of the same function,
// which would run the fold on both sides and hide a mutation) is what makes
// "behaves exactly as today" a real pin.
const naGenericCategoryAReason = "category-A (agent) failure — fishhawk_retry_stage retries it in place; read the trace first for transient harness errors"

// TestNextActions_UnmatchedFailureBehavesExactlyAsToday is the fail-open
// assertion: a failed stage whose reason matches no anchor carries NO
// signature, and its state / action names / retry reason are the values the
// classifier produced before the registry existed.
func TestNextActions_UnmatchedFailureBehavesExactlyAsToday(t *testing.T) {
	run := naRun("failed")
	impl := naFailedImplement("A", "the agent could not satisfy the binding assertion in step 4")
	stages := []Stage{naStage("plan", "succeeded"), impl}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	if na == nil {
		t.Fatal("nextActionsFor returned nil")
	}
	if na.Signature != nil {
		t.Fatalf("Signature = %+v, want nil on an unrecognized reason", na.Signature)
	}
	if na.State != "implement_failed_category_a" {
		t.Errorf("State = %q, want implement_failed_category_a", na.State)
	}
	wantActions := []string{"fishhawk_retry_stage", "fishhawk_revive_run"}
	if got := actionNames(na); !reflect.DeepEqual(got, wantActions) {
		t.Errorf("actions = %v, want %v", got, wantActions)
	}
	if got := findAction(t, na, "fishhawk_retry_stage").Reason; got != naGenericCategoryAReason {
		t.Errorf("retry reason = %q, want the pre-registry generic reason %q", got, naGenericCategoryAReason)
	}
}

// TestNextActions_MatchedFailureKeepsTodaysActions is the other half of the
// same claim: a MATCHED failure keeps the identical state / action names /
// retry reason. Only the additive signature block appears.
func TestNextActions_MatchedFailureKeepsTodaysActions(t *testing.T) {
	run := naRun("failed")
	run.RetryAttempt = 1
	stages := []Stage{
		naStage("plan", "succeeded"),
		naFailedStageWithProgress("A", "agent exited with exit status 1", 0, 0),
	}

	na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
	if na == nil || na.Signature == nil {
		t.Fatalf("want a matched signature, got %+v", na)
	}
	if na.State != "implement_failed_category_a" {
		t.Errorf("State = %q, want implement_failed_category_a", na.State)
	}
	wantActions := []string{"fishhawk_retry_stage", "fishhawk_revive_run"}
	if got := actionNames(na); !reflect.DeepEqual(got, wantActions) {
		t.Errorf("actions = %v, want %v — a match must not change the legal next moves", got, wantActions)
	}
	if got := findAction(t, na, "fishhawk_retry_stage").Reason; got != naGenericCategoryAReason {
		t.Errorf("retry reason = %q, want the unchanged generic reason %q", got, naGenericCategoryAReason)
	}
}

// naSignatureFixture is one evidence shape per catalog entry.
type naSignatureFixture struct {
	id     string
	run    *Run
	stages []Stage
}

func naSignatureFixtures() []naSignatureFixture {
	noProgressRun := naRun("failed")
	noProgressRun.RetryAttempt = 1
	return []naSignatureFixture{
		{id: "external_api_incident", run: naRun("failed"),
			stages: []Stage{naStage("plan", "succeeded"), naFailedImplement("A", "terminal external API error 529 (retries exhausted): exit status 1")}},
		{id: "model_quota_exhausted", run: naRun("failed"),
			stages: []Stage{naStage("plan", "succeeded"), naFailedImplement("A", "could not obtain model quota (likely a usage/rate cap): 0 tokens")}},
		{id: "slice_integration_conflict", run: naRun("failed"),
			stages: []Stage{naStage("plan", "succeeded"), naFailedImplement("B", "slice integration conflict: slice 2 could not merge")}},
		{id: "lineage_lock_contention", run: naRun("failed"),
			stages: []Stage{naStage("plan", "succeeded"), naFailedImplement("C", "lineage_lock: another runner holds the lineage lock")}},
		{id: "zero_exit_strand", run: naRun("failed"),
			stages: []Stage{naStage("plan", "succeeded"), naFailedImplement("D", "runner exited 0 without settling the stage (state=running)")}},
		{id: "runner_died_before_reporting", run: naRun("failed"),
			stages: []Stage{naStage("plan", "succeeded"), naFailedImplement("D", "runner exited 5 before reporting a terminal state")}},
		{id: "infra_flake_recurred", run: naRun("failed"),
			stages: []Stage{naStage("plan", "succeeded"), naFailedImplement("A", "verify gate failed after verify_infra_flake_retry absorbed one flake")}},
		{id: "agent_no_progress_repeat", run: noProgressRun,
			stages: []Stage{naStage("plan", "succeeded"), naFailedStageWithProgress("A", "agent exited with exit status 1", 0, 0)}},
	}
}

// TestNextActions_SignatureNeverMutatesActions is the counterfactual vehicle
// for the never-mutates-actions invariant, driven through foldFailureSignature
// DIRECTLY against a pristine sentinel action list.
//
// Going direct is deliberate: diffing two nextActionsFor calls would run the
// fold on BOTH sides, so a fold that prepended the playbook would appear in
// the baseline too and the test would stay green. The sentinel list is built
// in the test and never passes through the fold's producer, so the only thing
// that can change it is the fold itself.
//
// Every catalog entry is driven, so a new signature cannot ship without this
// assertion.
func TestNextActions_SignatureNeverMutatesActions(t *testing.T) {
	sentinel := []SuggestedAction{
		{Action: "fishhawk_retry_stage", Params: map[string]string{"stage_id": "s1"}, Precondition: "p1", Consumes: consumesRetryBudget, Reason: "r1"},
		{Action: "fishhawk_revive_run", Params: map[string]string{"run_id": "r1"}, Precondition: "p2", Consumes: consumesRetryBudget, Reason: "r2"},
	}

	covered := map[string]struct{}{}
	for _, f := range naSignatureFixtures() {
		t.Run(f.id, func(t *testing.T) {
			want := append([]SuggestedAction(nil), sentinel...)
			na := &NextActions{State: "sentinel_state", Actions: append([]SuggestedAction(nil), sentinel...)}

			foldFailureSignature(f.run, f.stages, na)

			if na.Signature == nil {
				t.Fatalf("fixture did not match a signature (want %s)", f.id)
			}
			if na.Signature.ID != f.id {
				t.Fatalf("Signature.ID = %q, want %q", na.Signature.ID, f.id)
			}
			covered[f.id] = struct{}{}

			if !reflect.DeepEqual(na.Actions, want) {
				t.Fatalf("the signature fold mutated the actions list:\n got %+v\nwant %+v", na.Actions, want)
			}
			if na.State != "sentinel_state" {
				t.Fatalf("the signature fold changed the state: %q", na.State)
			}
		})
	}

	for _, sig := range failuresig.Registry() {
		if _, ok := covered[sig.ID]; !ok {
			t.Errorf("catalog entry %q has no never-mutates-actions fixture", sig.ID)
		}
	}
}

// TestNextActions_SignatureFoldIsAdditiveEndToEnd complements the direct fold
// test: through the REAL classifier, every catalog fixture keeps a non-empty
// action list and gains only the block.
func TestNextActions_SignatureFoldIsAdditiveEndToEnd(t *testing.T) {
	for _, f := range naSignatureFixtures() {
		t.Run(f.id, func(t *testing.T) {
			na := nextActionsFor(f.run, f.stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
			if na == nil || na.Signature == nil {
				t.Fatalf("want a matched signature for %s, got %+v", f.id, na)
			}
			if na.Signature.ID != f.id {
				t.Fatalf("Signature.ID = %q, want %q", na.Signature.ID, f.id)
			}
			if len(na.Actions) == 0 {
				t.Fatal("the action list is empty — the block must never replace the actions")
			}
			for _, a := range na.Actions {
				if strings.Contains(a.Reason, na.Signature.Playbook[0]) {
					t.Fatalf("a playbook step leaked into action %q's reason", a.Action)
				}
			}
		})
	}
}

// TestNextActions_SignatureOnCIFailedPath pins that the ci_failed EARLY RETURN
// also carries the block — the fold is on both return paths.
func TestNextActions_SignatureOnCIFailedPath(t *testing.T) {
	run := naRun("running")
	prURL := "https://github.com/x/y/pull/42"
	run.PullRequestURL = &prURL
	stages := []Stage{
		naStage("plan", "succeeded"),
		naFailedImplement("A", "terminal external API error 529 (retries exhausted): exit status 1"),
	}
	drv := &DriveStatus{Drive: true, DerivedStatus: "ci_failed"}

	na := nextActionsFor(run, stages, nil, nil, nil, drv, false, false, false, "", "", releaseSignals{})
	if na == nil || na.Signature == nil {
		t.Fatalf("ci_failed path carried no signature (na=%+v)", na)
	}
	if na.Signature.ID != "external_api_incident" {
		t.Fatalf("Signature.ID = %q, want external_api_incident", na.Signature.ID)
	}
}

// TestNextActions_NoFailedStageNoSignature is a counterfactual vehicle for the
// nil-safety guard in failureSignatureFor: a healthy run must carry no
// signature, and an empty stage slice must not panic.
func TestNextActions_NoFailedStageNoSignature(t *testing.T) {
	t.Run("healthy run", func(t *testing.T) {
		run := naRun("running")
		stages := []Stage{naStage("plan", "succeeded"), naStage("implement", "running")}
		na := nextActionsFor(run, stages, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
		if na == nil {
			t.Fatal("nextActionsFor returned nil")
		}
		if na.Signature != nil {
			t.Fatalf("Signature = %+v, want nil on a run with no failed stage", na.Signature)
		}
	})
	t.Run("no stages at all", func(t *testing.T) {
		run := naRun("pending")
		na := nextActionsFor(run, nil, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{})
		if na == nil {
			t.Fatal("nextActionsFor returned nil")
		}
		if na.Signature != nil {
			t.Fatalf("Signature = %+v, want nil with no stages", na.Signature)
		}
	})
}

// TestNextActions_NilRunSafe pins the nil-run guards on BOTH surfaces. The
// direct failureSignatureFor call is deliberate: nextActionsFor returns early
// on a nil run, so only a direct call reaches that guard — deleting it turns
// this RED on a nil deref rather than leaving it vacuously green.
func TestNextActions_NilRunSafe(t *testing.T) {
	if na := nextActionsFor(nil, nil, nil, nil, nil, nil, false, false, false, "", "", releaseSignals{}); na != nil {
		t.Fatalf("nextActionsFor(nil run) = %+v, want nil", na)
	}
	stages := []Stage{naFailedImplement("A", "terminal external API error 529 (retries exhausted)")}
	if got := failureSignatureFor(nil, stages); got != nil {
		t.Fatalf("failureSignatureFor(nil run) = %+v, want nil", got)
	}
	// foldFailureSignature tolerates a nil block (the nil-run early return
	// upstream is the only producer, but the guard is the contract).
	foldFailureSignature(nil, stages, nil)
}

// TestNextActions_SignatureUsesFirstFailedStage pins the diagnosis-stage
// choice: stages are sequence-ordered, so the EARLIEST failure explains the
// run.
func TestNextActions_SignatureUsesFirstFailedStage(t *testing.T) {
	run := naRun("failed")
	stages := []Stage{
		naFailedImplement("A", "terminal external API error 529 (retries exhausted)"),
		naFailedImplement("A", "verify_infra_flake_retry recurred"),
	}
	got := failureSignatureFor(run, stages)
	if got == nil || got.ID != "external_api_incident" {
		t.Fatalf("failureSignatureFor = %+v, want external_api_incident from the FIRST failed stage", got)
	}
}

// TestFailureSignatureAnchorsMatchNextActionsPhrases pins that the classifier's
// failure-reason literals are SOURCED FROM the registry, not re-declared
// locally. A future local copy that drifts fails here loudly, rather than
// silently presenting as "no signature matched" while the surrounding action
// still fires.
func TestFailureSignatureAnchorsMatchNextActionsPhrases(t *testing.T) {
	if externalAPIReasonPhrase != failuresig.AnchorExternalAPIError {
		t.Errorf("externalAPIReasonPhrase = %q, registry anchor = %q", externalAPIReasonPhrase, failuresig.AnchorExternalAPIError)
	}
	if quotaUnavailableReasonPhrase != failuresig.AnchorQuotaUnavailable {
		t.Errorf("quotaUnavailableReasonPhrase = %q, registry anchor = %q", quotaUnavailableReasonPhrase, failuresig.AnchorQuotaUnavailable)
	}
	if sliceIntegrationConflictReasonPrefix != failuresig.AnchorSliceIntegrationConflict {
		t.Errorf("sliceIntegrationConflictReasonPrefix = %q, registry anchor = %q", sliceIntegrationConflictReasonPrefix, failuresig.AnchorSliceIntegrationConflict)
	}
	if len(flakeTraceEvents) != 1 || flakeTraceEvents[0] != failuresig.AnchorVerifyInfraFlake {
		t.Errorf("flakeTraceEvents = %v, registry anchor = %q", flakeTraceEvents, failuresig.AnchorVerifyInfraFlake)
	}
}
