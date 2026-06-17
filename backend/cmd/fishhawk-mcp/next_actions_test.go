package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
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
			name:         "amended_implement_pending_local_dispatch",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "pending")},
			wantState:    "implement_pending",
			wantActions:  []string{"fishhawk_run_stage"},
			wantConsumes: []string{consumesNone},
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
			name:         "e_category_a_retry_with_flake_citation",
			run:          naRun("failed"),
			stages:       []Stage{naStage("plan", "succeeded"), naFailedImplement("A", "verify failed after verify_infra_flake_retry absorbed one flake")},
			wantState:    "implement_failed_category_a",
			wantActions:  []string{"fishhawk_retry_stage"},
			wantConsumes: []string{consumesRetryBudget},
		},
		{
			name:         "e_category_a_retry_without_citation",
			run:          naRun("failed"),
			stages:       []Stage{naStage("plan", "succeeded"), naFailedImplement("A", "agent crashed")},
			wantState:    "implement_failed_category_a",
			wantActions:  []string{"fishhawk_retry_stage"},
			wantConsumes: []string{consumesRetryBudget},
		},
		{
			name:         "f_category_c_retry_or_cancel",
			run:          naRun("failed"),
			stages:       []Stage{naStage("plan", "succeeded"), naFailedImplement("C", "infra")},
			wantState:    "implement_failed",
			wantActions:  []string{"fishhawk_retry_stage", "fishhawk_cancel_run"},
			wantConsumes: []string{consumesRetryBudget, consumesNone},
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
			name:         "h_concerns_open_ceiling_reached",
			run:          naRun("running"),
			stages:       []Stage{naStage("plan", "succeeded"), naStage("implement", "awaiting_approval")},
			implRS:       naReviewStatus("implement", "complete"),
			hint:         &ReviewActionHint{Concerns: 1, RemainingFixupBudget: 0, OverrideAvailable: false},
			wantState:    "implement_concerns_open",
			wantActions:  []string{"merge_and_file_follow_up", "fishhawk_start_run"},
			wantConsumes: []string{consumesNone, consumesNewRun},
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
			na := nextActionsFor(tc.run, tc.stages, tc.planRS, tc.implRS, tc.hint, nil)
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

	na := nextActionsFor(run, stages, nil, nil, nil, nil)
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

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, drive)
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
		naFailedImplement("A", "verify aborted after verify_infra_flake_retry")}, nil, nil, nil, nil)
	retry := findAction(t, cited, "fishhawk_retry_stage")
	if !strings.Contains(retry.Reason, "verify_infra_flake_retry") {
		t.Errorf("retry reason should cite the flake trace event; got %q", retry.Reason)
	}

	uncited := nextActionsFor(run, []Stage{naStage("plan", "succeeded"),
		naFailedImplement("A", "agent crashed")}, nil, nil, nil, nil)
	retry = findAction(t, uncited, "fishhawk_retry_stage")
	if strings.Contains(retry.Reason, "verify_infra_flake_retry") {
		t.Errorf("retry reason cites a flake event the failure detail does not carry: %q", retry.Reason)
	}
}

// TestNextActions_ResumeRunNamesThisRunAsParent pins the #1022
// structural fix: the category-B recovery action's parent_run_id is
// THIS run's id (the failed run resume_run takes), so the remediation
// text can never go stale against a different run.
func TestNextActions_ResumeRunNamesThisRunAsParent(t *testing.T) {
	run := naRun("failed")
	na := nextActionsFor(run, []Stage{naStage("plan", "succeeded"),
		naFailedImplement("B", "undeclared created file")}, nil, nil, nil, nil)
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
		nil, naReviewStatus("implement", "pending"), nil, nil)
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
	if na := nextActionsFor(nil, nil, nil, nil, nil, nil); na != nil {
		t.Errorf("nextActionsFor(nil run) = %+v, want nil", na)
	}
}

// TestNextActions_PlanReviewPendingDoesNotOfferApproval pins the
// wait-for-the-agent-plan-review discipline: while the plan review is
// pending, the block must NOT contain an approval action.
func TestNextActions_PlanReviewPendingDoesNotOfferApproval(t *testing.T) {
	run := naRun("running")
	na := nextActionsFor(run, []Stage{naStage("plan", "awaiting_approval")},
		naReviewStatus("plan", "pending"), nil, nil, nil)
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

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), hint, drive)
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

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, drive)
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

	na := nextActionsFor(run, stages, nil, naReviewStatus("implement", "complete"), nil, drive)
	if na.Actions[0].Action != "classify_ci_failure" {
		t.Errorf("actions[0] = %q, want the drive next_action classify_ci_failure folded first", na.Actions[0].Action)
	}
}
