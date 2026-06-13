package issuecomment_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

const anchorExternalURL = "https://app.fishhawk.example.com"

var anchorNow = time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

// anchorRun returns a run + stages fixture in a mid-flight shape that
// tests mutate per scenario.
func anchorRun(runID uuid.UUID, planStageID uuid.UUID) (*run.Run, []*run.Stage) {
	r := &run.Run{
		ID:            runID,
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		TriggerSource: run.TriggerGitHubIssue,
		State:         run.StateRunning,
	}
	stages := []*run.Stage{
		{ID: planStageID, RunID: runID, Sequence: 1, Type: run.StageTypePlan, State: run.StageStateSucceeded},
		{ID: uuid.New(), RunID: runID, Sequence: 2, Type: run.StageTypeImplement, State: run.StageStateRunning},
		{ID: uuid.New(), RunID: runID, Sequence: 3, Type: run.StageTypeReview, State: run.StageStatePending},
	}
	return r, stages
}

func anchorAudit(runID uuid.UUID, seq int64, category, actor string, stageID *uuid.UUID, ts time.Time, payload map[string]any) *audit.Entry {
	rid := runID
	body, _ := json.Marshal(payload)
	var actorPtr *string
	if actor != "" {
		actorPtr = &actor
	}
	return &audit.Entry{
		ID:           uuid.New(),
		Sequence:     seq,
		RunID:        &rid,
		StageID:      stageID,
		Timestamp:    ts,
		Category:     category,
		ActorSubject: actorPtr,
		Payload:      body,
	}
}

func samplePlan(summary string) *plan.Plan {
	return &plan.Plan{
		PlanVersion: "standard_v1",
		Summary:     summary,
		Scope: plan.Scope{Files: []plan.ScopeFile{
			{Path: "backend/internal/issuecomment/anchor_template.go", Operation: plan.FileOpCreate},
		}},
		Approach: []plan.ApproachStep{{Step: 1, Description: "Add the renderer."}},
		Verification: plan.Verification{
			TestStrategy: "Unit tests for each rung.",
			RollbackPlan: "Single revert.",
		},
		RisksAndAssumptions: []string{"GitHub caps comment bodies at 65536 bytes."},
	}
}

func TestRenderAnchorBody_NilRun(t *testing.T) {
	if got := issuecomment.RenderAnchorBody(nil, nil, nil, nil, anchorExternalURL, anchorNow); got != "" {
		t.Fatalf("nil run should render empty, got %q", got)
	}
}

func TestRenderAnchorBody_HeaderAndFooter(t *testing.T) {
	runID := uuid.MustParse("7be5974b-c389-4577-a5a9-43510cadca88")
	planStage := uuid.New()
	r, stages := anchorRun(runID, planStage)
	prURL := "https://github.com/kuhlman-labs/fishhawk/pull/9"
	r.PullRequestURL = &prURL

	body := issuecomment.RenderAnchorBody(r, stages, nil, nil, anchorExternalURL, anchorNow)

	for _, want := range []string{
		"Fishhawk run",
		"7be5974b",
		anchorExternalURL + "/runs/" + runID.String(),
		"feature_change",
		"running",
		"[View run →]",
		"[Pull request →](" + prURL + ")",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n---\n%s", want, body)
		}
	}
}

func TestRenderAnchorBody_WhatNowAwaitingApproval(t *testing.T) {
	runID := uuid.New()
	planStage := uuid.New()
	r, stages := anchorRun(runID, planStage)
	stages[0].State = run.StageStateAwaitingApproval

	body := issuecomment.RenderAnchorBody(r, stages, nil, nil, anchorExternalURL, anchorNow)

	for _, want := range []string{"What now:", "awaiting approval", "`+1`", "`lgtm`"} {
		if !strings.Contains(body, want) {
			t.Errorf("awaiting-approval what-now missing %q\n---\n%s", want, body)
		}
	}
}

func TestRenderAnchorBody_WhatNowReviewsPending(t *testing.T) {
	runID := uuid.New()
	planStage := uuid.New()
	r, stages := anchorRun(runID, planStage)
	// Two reviewers configured; only one terminal verdict landed.
	chain := []*audit.Entry{
		anchorAudit(runID, 1, "plan_review_started", "", &planStage, anchorNow.Add(-10*time.Minute),
			map[string]any{"configured_agents": 2}),
		anchorAudit(runID, 2, "plan_reviewed", "", &planStage, anchorNow.Add(-9*time.Minute),
			map[string]any{"reviewer_model": "opus-4-8", "verdict": "approve"}),
	}

	body := issuecomment.RenderAnchorBody(r, stages, nil, chain, anchorExternalURL, anchorNow)

	if !strings.Contains(body, "waiting on 1 reviewer verdict.") {
		t.Errorf("reviews-pending what-now missing singular count\n---\n%s", body)
	}
}

func TestRenderAnchorBody_WhatNowFailedCategory(t *testing.T) {
	runID := uuid.New()
	planStage := uuid.New()
	r, stages := anchorRun(runID, planStage)
	r.State = run.StateFailed
	catB := run.FailureB
	stages[1].State = run.StageStateFailed
	stages[1].FailureCategory = &catB

	body := issuecomment.RenderAnchorBody(r, stages, nil, nil, anchorExternalURL, anchorNow)

	for _, want := range []string{"What now:", "category B", "constraint or policy violation"} {
		if !strings.Contains(body, want) {
			t.Errorf("failed what-now missing %q\n---\n%s", want, body)
		}
	}
}

func TestRenderAnchorBody_TimelineApproverIdentityThreeForms(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		actor   string
		want    string
	}{
		{
			name:    "github login mention",
			payload: map[string]any{"decision": "approve", "approver_github_login": "brettk"},
			want:    "@brettk approved the plan",
		},
		{
			name:    "operator agent token subject",
			payload: map[string]any{"decision": "approve", "approver": "operator-agent/operator-role-v0", "delegated": "clean_dual_approval"},
			want:    "the operator agent (`operator-agent/operator-role-v0`, delegated: `clean_dual_approval`) approved the plan",
		},
		{
			name:    "non-login subject verbatim code span",
			payload: map[string]any{"decision": "reject", "approver": "brett@local-mcp", "rejection_comment": "wrong fork"},
			want:    "`brett@local-mcp` rejected the plan",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := uuid.New()
			planStage := uuid.New()
			r, stages := anchorRun(runID, planStage)
			chain := []*audit.Entry{
				anchorAudit(runID, 1, "approval_submitted", tc.actor, &planStage, anchorNow.Add(-time.Minute), tc.payload),
			}
			body := issuecomment.RenderAnchorBody(r, stages, nil, chain, anchorExternalURL, anchorNow)
			if !strings.Contains(body, tc.want) {
				t.Errorf("timeline missing %q\n---\n%s", tc.want, body)
			}
		})
	}
}

func TestRenderAnchorBody_TimelineRejectionReasonShown(t *testing.T) {
	runID := uuid.New()
	planStage := uuid.New()
	r, stages := anchorRun(runID, planStage)
	chain := []*audit.Entry{
		anchorAudit(runID, 1, "approval_submitted", "brettk", &planStage, anchorNow.Add(-time.Minute),
			map[string]any{"decision": "reject", "approver_github_login": "brettk", "rejection_comment": "scope is wrong"}),
	}
	body := issuecomment.RenderAnchorBody(r, stages, nil, chain, anchorExternalURL, anchorNow)
	if !strings.Contains(body, "scope is wrong") {
		t.Errorf("rejection reason not surfaced in timeline\n---\n%s", body)
	}
}

func TestRenderAnchorBody_TimelineVerdictSeverityAndFreeForm(t *testing.T) {
	runID := uuid.New()
	planStage := uuid.New()
	r, stages := anchorRun(runID, planStage)
	chain := []*audit.Entry{
		anchorAudit(runID, 1, "implement_reviewed", "", &planStage, anchorNow.Add(-2*time.Minute),
			map[string]any{
				"reviewer_model": "gpt-5.5",
				"verdict":        "reject",
				"free_form":      "The seam between wire and persist is untested.",
				"concerns": []map[string]any{
					{"severity": "high", "category": "correctness", "note": "x"},
					{"severity": "high", "category": "scope", "note": "y"},
					{"severity": "medium", "category": "style", "note": "z"},
				},
			}),
	}
	body := issuecomment.RenderAnchorBody(r, stages, nil, chain, anchorExternalURL, anchorNow)

	for _, want := range []string{
		"`gpt-5.5`: rejected (2 high, 1 medium)",
		"<details><summary>reviewer notes</summary>",
		"The seam between wire and persist is untested.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("verdict block missing %q\n---\n%s", want, body)
		}
	}
}

func TestRenderAnchorBody_TimelineChronologicalRegardlessOfInputOrder(t *testing.T) {
	runID := uuid.New()
	planStage := uuid.New()
	r, stages := anchorRun(runID, planStage)
	// Supplied newest-first; renderer must sort ascending.
	chain := []*audit.Entry{
		anchorAudit(runID, 3, "approval_submitted", "brettk", &planStage, anchorNow.Add(-time.Minute),
			map[string]any{"decision": "approve", "approver_github_login": "brettk"}),
		anchorAudit(runID, 1, "run_dispatched", "", nil, anchorNow.Add(-10*time.Minute), nil),
	}
	body := issuecomment.RenderAnchorBody(r, stages, nil, chain, anchorExternalURL, anchorNow)
	dispatchIdx := strings.Index(body, "Run dispatched")
	approveIdx := strings.Index(body, "approved the plan")
	if dispatchIdx < 0 || approveIdx < 0 {
		t.Fatalf("timeline missing entries\n---\n%s", body)
	}
	if dispatchIdx > approveIdx {
		t.Errorf("expected dispatch before approval in chronological order\n---\n%s", body)
	}
}

func TestRenderAnchorBody_CurrentPlanCollapsedWithVisibleSummary(t *testing.T) {
	runID := uuid.New()
	planStage := uuid.New()
	r, stages := anchorRun(runID, planStage)
	versions := []issuecomment.PlanVersion{
		{Plan: samplePlan("Add the anchor renderer."), Version: 1, StageID: planStage, RequiresApproval: true},
	}
	body := issuecomment.RenderAnchorBody(r, stages, versions, nil, anchorExternalURL, anchorNow)

	for _, want := range []string{
		"<details><summary><b>Plan v1</b> — Add the anchor renderer.</summary>",
		"**Approach**",
		"Add the renderer.",
		fmt.Sprintf("[Approve in the dashboard →](%s/runs/%s/stages/%s)", anchorExternalURL, runID.String(), planStage.String()),
		"`+1`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("current plan missing %q\n---\n%s", want, body)
		}
	}
}

func TestRenderAnchorBody_SupersededPlanCollapsedWithRejectionReason(t *testing.T) {
	runID := uuid.New()
	planStageV1 := uuid.New()
	planStageV2 := uuid.New()
	r, stages := anchorRun(runID, planStageV2)
	versions := []issuecomment.PlanVersion{
		{Plan: samplePlan("First attempt."), Version: 1, StageID: planStageV1, Superseded: true},
		{Plan: samplePlan("Second attempt."), Version: 2, StageID: planStageV2},
	}
	chain := []*audit.Entry{
		anchorAudit(runID, 1, "approval_submitted", "brettk", &planStageV1, anchorNow.Add(-time.Hour),
			map[string]any{"decision": "reject", "approver_github_login": "brettk", "rejection_comment": "needs rework"}),
	}
	body := issuecomment.RenderAnchorBody(r, stages, versions, chain, anchorExternalURL, anchorNow)

	for _, want := range []string{
		"**Earlier plans**",
		"Plan v1 (superseded) — rejected: needs rework",
		"Plan v2", // current
		"Second attempt.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("superseded plan render missing %q\n---\n%s", want, body)
		}
	}
}

// --- Degradation ladder ----------------------------------------------

// bigPlan returns a plan whose body alone is well over the cap so the
// ladder must shed everything droppable.
func bigPlan(summary string, bodyBytes int) *plan.Plan {
	p := samplePlan(summary)
	p.RisksAndAssumptions = []string{strings.Repeat("x", bodyBytes)}
	return p
}

func TestRenderAnchorBody_LadderDropsOldestTimelineFirst(t *testing.T) {
	runID := uuid.New()
	planStage := uuid.New()
	r, stages := anchorRun(runID, planStage)
	// Many bulky free_form verdicts so the timeline alone busts the cap.
	chain := make([]*audit.Entry, 0, 40)
	bigFreeForm := strings.Repeat("y", 4000)
	for i := 0; i < 40; i++ {
		chain = append(chain, anchorAudit(runID, int64(i+1), "plan_reviewed", "", &planStage,
			anchorNow.Add(time.Duration(i)*time.Minute),
			map[string]any{
				"reviewer_model": fmt.Sprintf("model-%02d", i),
				"verdict":        "approve",
				"free_form":      bigFreeForm,
			}))
	}
	versions := []issuecomment.PlanVersion{
		{Plan: samplePlan("Compact current plan."), Version: 1, StageID: planStage, RequiresApproval: true},
	}
	body := issuecomment.RenderAnchorBody(r, stages, versions, chain, anchorExternalURL, anchorNow)

	if len(body) > issuecomment.MaxIssueCommentBodyBytes {
		t.Fatalf("body exceeds cap: %d > %d", len(body), issuecomment.MaxIssueCommentBodyBytes)
	}
	// Invariants that must always survive.
	for _, want := range []string{"Fishhawk run", "Compact current plan.", "[View run →]"} {
		if !strings.Contains(body, want) {
			t.Errorf("ladder dropped a load-bearing section %q\n---\n%s", want, body[:min(len(body), 600)])
		}
	}
	// The newest verdict (model-39) should be retained over the oldest
	// (model-00) when the timeline is partially dropped.
	if strings.Contains(body, "earlier events omitted") {
		if strings.Contains(body, "model-00") && !strings.Contains(body, "model-39") {
			t.Errorf("ladder kept oldest timeline entry over newest\n---\n%s", body[:min(len(body), 600)])
		}
	}
}

func TestRenderAnchorBody_LadderDropsSupersededBeforeCurrentSummary(t *testing.T) {
	runID := uuid.New()
	planStageV1 := uuid.New()
	planStageV2 := uuid.New()
	r, stages := anchorRun(runID, planStageV2)
	versions := []issuecomment.PlanVersion{
		{Plan: bigPlan("Superseded huge plan.", 40000), Version: 1, StageID: planStageV1, Superseded: true},
		{Plan: bigPlan("Current huge plan.", 40000), Version: 2, StageID: planStageV2, RequiresApproval: true},
	}
	body := issuecomment.RenderAnchorBody(r, stages, versions, nil, anchorExternalURL, anchorNow)

	if len(body) > issuecomment.MaxIssueCommentBodyBytes {
		t.Fatalf("body exceeds cap: %d > %d", len(body), issuecomment.MaxIssueCommentBodyBytes)
	}
	// Current plan SUMMARY survives; the superseded plan is dropped.
	if !strings.Contains(body, "Current huge plan.") {
		t.Errorf("current plan summary dropped under pressure\n---\n%s", body[:min(len(body), 600)])
	}
	if !strings.Contains(body, "Fishhawk run") || !strings.Contains(body, "[View run →]") {
		t.Errorf("header or footer dropped under pressure\n---\n%s", body[:min(len(body), 600)])
	}
}

func TestRenderAnchorBody_LadderCollapsesCurrentPlanToSummary(t *testing.T) {
	runID := uuid.New()
	planStage := uuid.New()
	r, stages := anchorRun(runID, planStage)
	// A single plan whose expanded body is enormous forces the ladder
	// down to its last structured rung: the current plan collapsed to
	// its (length-capped) summary line, which always fits under the cap.
	huge := strings.Repeat("z", issuecomment.MaxIssueCommentBodyBytes*2)
	versions := []issuecomment.PlanVersion{
		{Plan: samplePlan(huge), Version: 1, StageID: planStage, RequiresApproval: true},
	}
	body := issuecomment.RenderAnchorBody(r, stages, versions, nil, anchorExternalURL, anchorNow)
	if len(body) > issuecomment.MaxIssueCommentBodyBytes {
		t.Fatalf("collapsed body exceeds cap: %d > %d", len(body), issuecomment.MaxIssueCommentBodyBytes)
	}
	for _, want := range []string{"Fishhawk run", "**Plan v1** —", "[View run →]"} {
		if !strings.Contains(body, want) {
			t.Errorf("collapsed rung dropped %q\n---\n%s", want, body[:min(len(body), 400)])
		}
	}
	// The expanded <details> body must be gone at this rung.
	if strings.Contains(body, "<details><summary><b>Plan v1</b>") {
		t.Errorf("current plan still expanded at the collapse rung\n---\n%s", body[:min(len(body), 400)])
	}
}

// (min is the Go builtin; no local helper needed.)

func TestRenderAnchorBody_IdempotentRebuild(t *testing.T) {
	runID := uuid.New()
	planStage := uuid.New()
	r, stages := anchorRun(runID, planStage)
	versions := []issuecomment.PlanVersion{
		{Plan: samplePlan("Stable plan."), Version: 1, StageID: planStage, RequiresApproval: true},
	}
	chain := []*audit.Entry{
		anchorAudit(runID, 1, "run_dispatched", "", nil, anchorNow.Add(-5*time.Minute), nil),
		anchorAudit(runID, 2, "approval_submitted", "brettk", &planStage, anchorNow.Add(-time.Minute),
			map[string]any{"decision": "approve", "approver_github_login": "brettk"}),
	}
	first := issuecomment.RenderAnchorBody(r, stages, versions, chain, anchorExternalURL, anchorNow)
	second := issuecomment.RenderAnchorBody(r, stages, versions, chain, anchorExternalURL, anchorNow)
	if first != second {
		t.Errorf("anchor render not idempotent for identical inputs\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}
