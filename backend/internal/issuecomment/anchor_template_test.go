package issuecomment

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func anchorRun() *run.Run {
	return &run.Run{
		ID:         uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		WorkflowID: "feature_change",
		State:      run.StateRunning,
	}
}

func reviewedEntry(t *testing.T, seq int64, stageType, model, verdict string, concerns []anchorReviewConcern, freeForm string) *audit.Entry {
	t.Helper()
	cs := make([]map[string]string, 0, len(concerns))
	for _, c := range concerns {
		cs = append(cs, map[string]string{"severity": c.severity, "category": c.category, "note": c.note})
	}
	payload, _ := json.Marshal(map[string]any{
		"reviewer_model": model,
		"verdict":        verdict,
		"free_form":      freeForm,
		"concerns":       cs,
	})
	return &audit.Entry{Sequence: seq, Category: stageType + "_reviewed", Payload: payload, Timestamp: time.Unix(seq, 0).UTC()}
}

func startedEntry(seq int64, stageType string) *audit.Entry {
	return &audit.Entry{Sequence: seq, Category: stageType + "_review_started", Timestamp: time.Unix(seq, 0).UTC()}
}

// TestRenderAnchorBody_SliceIntegrationTimeline pins #1147: the fan-in
// outcome audit kinds surface in the living-anchor timeline (which reuses
// pickActivity + renderActivityLine), so a decomposed parent's clean
// integration / conflict shows up in the anchor.
func TestRenderAnchorBody_SliceIntegrationTimeline(t *testing.T) {
	cases := []struct {
		category string
		want     string
	}{
		{"slices_integrated", "Slices integrated"},
		{"slice_integration_conflict", "Slice integration conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.category, func(t *testing.T) {
			entries := []*audit.Entry{
				{Sequence: 5, Category: tc.category, Timestamp: time.Unix(5, 0).UTC()},
			}
			body := RenderAnchorBody(AnchorInput{
				Run:         anchorRun(),
				Stages:      []*run.Stage{{Type: run.StageTypeImplement, State: run.StageStateRunning}},
				Audit:       entries,
				ExternalURL: "https://app.example",
				Now:         time.Unix(1000, 0).UTC(),
			})
			if !strings.Contains(body, tc.want) {
				t.Errorf("anchor timeline missing %q:\n%s", tc.want, body)
			}
		})
	}
}

// TestRenderAnchorBody_DeployTimeline pins E23.5 / #1385: the deploy
// governance audit kinds surface on the living-anchor timeline (which reuses
// pickActivity + renderActivityLine), so a completed deploy's outcome renders
// in the anchor with no anchor-specific rendering code.
func TestRenderAnchorBody_DeployTimeline(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"environment": "production", "outcome": "succeeded"})
	entries := []*audit.Entry{
		{Sequence: 7, Category: "deployment_outcome_recorded", Payload: payload, Timestamp: time.Unix(7, 0).UTC()},
	}
	body := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypeDeploy, State: run.StageStateRunning}},
		Audit:       entries,
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	})
	if !strings.Contains(body, "Deployed to `production` — succeeded") {
		t.Errorf("anchor timeline missing the deploy outcome:\n%s", body)
	}
}

// TestRenderAnchorBody_AcceptanceTimeline pins E31.3 / #1531: the acceptance
// evidence audit kinds surface on the living-anchor timeline (which reuses
// pickActivity + renderActivityLine), so a rebuilt anchor's recent-activity
// block shows the acceptance outcome with no anchor-specific rendering code —
// the issue's "anchor rebuild from the audit chain shows the acceptance
// outcome" criterion, exercised through the real anchor build path.
func TestRenderAnchorBody_AcceptanceTimeline(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"outcome": "accepted", "criteria_passed": 3, "criteria_total": 4})
	entries := []*audit.Entry{
		{Sequence: 8, Category: "acceptance_outcome_recorded", Payload: payload, Timestamp: time.Unix(8, 0).UTC()},
	}
	body := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypeAcceptance, State: run.StageStateRunning}},
		Audit:       entries,
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	})
	if !strings.Contains(body, "Acceptance recorded — accepted (3/4 criteria passed)") {
		t.Errorf("anchor timeline missing the acceptance outcome:\n%s", body)
	}
}

// TestRenderAnchorBody_ModelRecommendationAndResolved pins #1013: the anchor
// renders the plan's model_recommendation (implement_model + rationale) under
// the plan, and the gate's resolved model_resolved {value, source} as a
// dedicated block.
func TestRenderAnchorBody_ModelRecommendationAndResolved(t *testing.T) {
	resolved, _ := json.Marshal(map[string]any{"model": "claude-opus-4-8", "model_source": "operator"})
	body := RenderAnchorBody(AnchorInput{
		Run:    anchorRun(),
		Stages: []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateSucceeded}},
		CurrentPlan: &AnchorPlanView{
			Summary:                 "Resolve the implement model at the gate.",
			RecommendedModel:        "claude-sonnet-4-6",
			RecommendationRationale: "medium complexity",
		},
		Audit: []*audit.Entry{
			{Sequence: 9, Category: "model_resolved", Payload: resolved, Timestamp: time.Unix(9, 0).UTC()},
		},
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	})
	if !strings.Contains(body, "Model recommendation: `claude-sonnet-4-6`") {
		t.Errorf("anchor should render the plan model recommendation: %q", body)
	}
	if !strings.Contains(body, "medium complexity") {
		t.Errorf("anchor should render the recommendation rationale: %q", body)
	}
	if !strings.Contains(body, "**Implement model** — `claude-opus-4-8` (source: operator)") {
		t.Errorf("anchor should render the resolved model block: %q", body)
	}
}

// TestRenderAnchorBody_ModelResolvedEmptyDefaultSpawn covers the empty
// resolution: the gate recorded a model_resolved with no model (the deliberate
// default spawn), and the anchor states it honestly rather than omitting it.
func TestRenderAnchorBody_ModelResolvedEmptyDefaultSpawn(t *testing.T) {
	resolved, _ := json.Marshal(map[string]any{"model": "", "model_source": ""})
	body := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypeImplement, State: run.StageStateRunning}},
		Audit:       []*audit.Entry{{Sequence: 3, Category: "model_resolved", Payload: resolved, Timestamp: time.Unix(3, 0).UTC()}},
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	})
	if !strings.Contains(body, "**Implement model** — adapter default") {
		t.Errorf("anchor should render the empty resolution as adapter default: %q", body)
	}
}

func TestRenderAnchorBody_HeaderAndWhatNow(t *testing.T) {
	body := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}},
		ExternalURL: "https://app.example/",
		Now:         time.Now(),
	})
	if !strings.Contains(body, "Fishhawk run") {
		t.Errorf("missing header: %q", body)
	}
	if !strings.Contains(body, "feature_change") {
		t.Errorf("missing workflow id: %q", body)
	}
	if !strings.Contains(body, "awaiting approval") {
		t.Errorf("what-now should surface awaiting-approval: %q", body)
	}
	if !strings.Contains(body, "https://app.example/runs/11111111") {
		t.Errorf("missing run deep-link: %q", body)
	}
}

func TestRenderAnchorBody_CurrentAndSupersededPlans(t *testing.T) {
	body := RenderAnchorBody(AnchorInput{
		Run:    anchorRun(),
		Stages: []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateRunning}},
		CurrentPlan: &AnchorPlanView{
			Summary:  "Add the living anchor comment.",
			Files:    []plan.ScopeFile{{Path: "a.go", Operation: "modify"}},
			Approach: []plan.ApproachStep{{Step: 1, Description: "do the thing"}},
		},
		SupersededPlans: []AnchorPlanView{
			{Summary: "First attempt.", RejectionReason: "wrong fork"},
		},
		ExternalURL: "https://app.example",
		Now:         time.Now(),
	})
	if !strings.Contains(body, "**Plan**") {
		t.Errorf("current plan should have a visible **Plan** heading: %q", body)
	}
	if !strings.Contains(body, "Add the living anchor comment.") {
		t.Errorf("current plan summary should be visible plain markdown: %q", body)
	}
	if !strings.Contains(body, "<details><summary>Plan details</summary>") {
		t.Errorf("current plan scope/approach should be inside a Plan details block: %q", body)
	}
	if !strings.Contains(body, "`a.go`") {
		t.Errorf("current plan scope file should render: %q", body)
	}
	// The summary must NOT be buried inside the <summary> attribute anymore.
	if strings.Contains(body, "<summary>📋 Plan") {
		t.Errorf("current plan must not use the old summary-in-<summary> form: %q", body)
	}
	if strings.Contains(body, "<summary>Add the living anchor comment.") {
		t.Errorf("plan summary text must not appear inside a <summary> tag: %q", body)
	}
	if !strings.Contains(body, "Superseded plan — First attempt.") {
		t.Errorf("superseded plan should render collapsed: %q", body)
	}
	if !strings.Contains(body, "Rejected: wrong fork") {
		t.Errorf("superseded plan should carry its rejection reason: %q", body)
	}
}

func TestRenderAnchorBody_ReviewVerdictsInline(t *testing.T) {
	entries := []*audit.Entry{
		startedEntry(10, "plan"),
		reviewedEntry(t, 11, "plan", "claude-opus-4-8", "approve", nil, ""),
		reviewedEntry(t, 12, "plan", "gpt-5.5", "reject",
			[]anchorReviewConcern{{severity: "high", category: "correctness", note: "boom"}}, "see the note"),
	}
	body := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}},
		Audit:       entries,
		ExternalURL: "https://app.example",
		Now:         time.Now(),
	})
	if !strings.Contains(body, "claude-opus-4-8: approve") {
		t.Errorf("opus verdict missing: %q", body)
	}
	if !strings.Contains(body, "gpt-5.5: reject (1 high)") {
		t.Errorf("codex verdict with severity-tagged concern count missing: %q", body)
	}
	if !strings.Contains(body, "see the note") {
		t.Errorf("free_form should be in the expandable details: %q", body)
	}
}

// TestRenderAnchorBody_PerReviewerBlocks pins the per-reviewer-block-count
// shape (#1073): every current-round reviewer gets its own <details>, even
// a bare approve with no concerns and no free_form, so a two-reviewer round
// can never read as one.
func TestRenderAnchorBody_PerReviewerBlocks(t *testing.T) {
	entries := []*audit.Entry{
		startedEntry(10, "plan"),
		reviewedEntry(t, 11, "plan", "claude-opus-4-8", "approve", nil, ""),
		reviewedEntry(t, 12, "plan", "gpt-5.5", "approve", nil, ""),
	}
	body := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}},
		Audit:       entries,
		ExternalURL: "https://app.example",
		Now:         time.Now(),
	})
	if !strings.Contains(body, "<details><summary>claude-opus-4-8: approve</summary>") {
		t.Errorf("opus per-reviewer block missing: %q", body)
	}
	if !strings.Contains(body, "<details><summary>gpt-5.5: approve</summary>") {
		t.Errorf("codex per-reviewer block missing: %q", body)
	}
	if n := strings.Count(body, "(no additional notes)"); n != 2 {
		t.Errorf("expected exactly 2 '(no additional notes)' bodies; got %d: %q", n, body)
	}
	if !strings.Contains(body, "claude-opus-4-8: approve · gpt-5.5: approve") {
		t.Errorf("inline one-liner must survive: %q", body)
	}
}

// TestRenderAnchorBody_StaleVerdictExcluded is the binding-condition-1
// acceptance test: a verdict from a prior review round (Sequence below
// the latest *_review_started) must NOT read as the current round.
func TestRenderAnchorBody_StaleVerdictExcluded(t *testing.T) {
	entries := []*audit.Entry{
		// Round 1: opus rejected.
		startedEntry(5, "implement"),
		reviewedEntry(t, 6, "implement", "claude-opus-4-8", "reject",
			[]anchorReviewConcern{{severity: "high", category: "correctness", note: "round-1 problem"}}, "stale free-form"),
		// A fixup re-opened the stage; round 2 dispatched anew.
		startedEntry(20, "implement"),
		reviewedEntry(t, 21, "implement", "claude-opus-4-8", "approve", nil, "now good"),
	}
	verdicts := currentRoundReviewVerdicts("implement", entries)
	if len(verdicts) != 1 {
		t.Fatalf("expected exactly 1 current-round verdict; got %d: %+v", len(verdicts), verdicts)
	}
	if verdicts[0].verdict != "approve" {
		t.Errorf("current-round verdict should be the round-2 approve; got %q", verdicts[0].verdict)
	}

	body := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypeImplement, State: run.StageStateRunning}},
		Audit:       entries,
		ExternalURL: "https://app.example",
		Now:         time.Now(),
	})
	if strings.Contains(body, "round-1 problem") || strings.Contains(body, "stale free-form") {
		t.Errorf("stale round-1 verdict leaked into the anchor: %q", body)
	}
	if !strings.Contains(body, "claude-opus-4-8: approve") {
		t.Errorf("current-round approve missing: %q", body)
	}
}

func approvalEntry(t *testing.T, seq int64, login, decision, comment string) *audit.Entry {
	t.Helper()
	payload := map[string]any{
		"decision":              decision,
		"approver_github_login": login,
	}
	if decision == "approve" && comment != "" {
		payload["comment"] = comment
	}
	if decision == "reject" && comment != "" {
		payload["rejection_comment"] = comment
	}
	raw, _ := json.Marshal(payload)
	return &audit.Entry{Sequence: seq, Category: "approval_submitted", Payload: raw, Timestamp: time.Unix(seq, 0).UTC()}
}

// TestRenderAnchorBody_GateDecisionTimeline covers the enriched
// gate-decision timeline entry (#1070): the decision phrase, the
// conditions <details>, and the "over N advisory reject(s)" arbitration
// marker — each only when the underlying chain warrants it.
func TestRenderAnchorBody_GateDecisionTimeline(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	tests := []struct {
		name        string
		entries     []*audit.Entry
		wantContain []string
		wantAbsent  []string
	}{
		{
			name: "approve with conditions",
			entries: []*audit.Entry{
				approvalEntry(t, 5, "alice", "approve", "keep the two-round test"),
			},
			wantContain: []string{
				"@alice approved the plan with conditions",
				"<details><summary>Approval conditions</summary>",
				"keep the two-round test",
			},
			wantAbsent: []string{"advisory reject"},
		},
		{
			name: "approve over one advisory reject",
			entries: []*audit.Entry{
				startedEntry(10, "plan"),
				reviewedEntry(t, 11, "plan", "claude-opus-4-8", "approve", nil, ""),
				reviewedEntry(t, 12, "plan", "gpt-5.5", "reject",
					[]anchorReviewConcern{{severity: "high", category: "correctness", note: "boom"}}, "see note"),
				approvalEntry(t, 13, "alice", "approve", ""),
			},
			wantContain: []string{"@alice approved the plan (over 1 advisory reject)"},
			wantAbsent:  []string{"Approval conditions", "over 2 advisory"},
		},
		{
			name: "clean approve",
			entries: []*audit.Entry{
				approvalEntry(t, 5, "alice", "approve", ""),
			},
			wantContain: []string{"@alice approved the plan"},
			wantAbsent:  []string{"advisory reject", "Approval conditions", "with conditions"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := RenderAnchorBody(AnchorInput{
				Run:         anchorRun(),
				Stages:      []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateRunning}},
				Audit:       tt.entries,
				ExternalURL: "https://app.example",
				Now:         now,
			})
			for _, want := range tt.wantContain {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q:\n%s", want, body)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("body should not contain %q:\n%s", absent, body)
				}
			}
		})
	}
}

// TestAdvisoryRejectCountBefore_ReplanRoundBound is load-bearing per the
// approval conditions: a second approval round counts only its OWN
// round's reviewer rejects, never the prior round's — the bound between
// the approval Sequence and the latest preceding plan_review_started.
func TestAdvisoryRejectCountBefore_ReplanRoundBound(t *testing.T) {
	entries := []*audit.Entry{
		// Round 1: a reject, then the operator rejects the plan (replan).
		startedEntry(5, "plan"),
		reviewedEntry(t, 6, "plan", "gpt-5.5", "reject", nil, "round-1 concern"),
		approvalEntry(t, 7, "alice", "reject", "replan please"),
		// Round 2: a reject, then the operator approves OVER it.
		startedEntry(20, "plan"),
		reviewedEntry(t, 21, "plan", "gpt-5.5", "reject", nil, "round-2 concern"),
		reviewedEntry(t, 22, "plan", "claude-opus-4-8", "approve", nil, ""),
		approvalEntry(t, 23, "alice", "approve", ""),
	}
	if n := advisoryRejectCountBefore("plan", entries, 23); n != 1 {
		t.Fatalf("round-2 approval should count only its own round's 1 reject; got %d", n)
	}

	body := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateRunning}},
		Audit:       entries,
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	})
	if !strings.Contains(body, "over 1 advisory reject") {
		t.Errorf("round-2 approve should show 'over 1 advisory reject':\n%s", body)
	}
	if strings.Contains(body, "over 2 advisory") {
		t.Errorf("round-2 approve must not over-count round-1 rejects:\n%s", body)
	}
}

func TestRenderAnchorBody_DegradationLadder(t *testing.T) {
	// Build an oversized synthetic chain: many timeline rows + a huge
	// superseded plan + a huge current plan. The ladder must keep the
	// body under the cap while preserving the header, the current plan
	// summary, and the dashboard deep-link.
	big := strings.Repeat("x", MaxIssueCommentBodyBytes/2)
	var entries []*audit.Entry
	for i := int64(1); i <= 200; i++ {
		entries = append(entries, &audit.Entry{
			Sequence: i, Category: "plan_generated", Timestamp: time.Unix(i, 0).UTC(),
		})
	}
	body := RenderAnchorBody(AnchorInput{
		Run:    anchorRun(),
		Stages: []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateRunning}},
		Audit:  entries,
		CurrentPlan: &AnchorPlanView{
			Summary: "Current plan summary stays.",
			Files:   []plan.ScopeFile{{Path: big, Operation: "modify"}},
		},
		SupersededPlans: []AnchorPlanView{
			{Summary: "Old plan", Files: []plan.ScopeFile{{Path: big, Operation: "modify"}}},
		},
		ExternalURL: "https://app.example",
		Now:         time.Now(),
	})
	if len(body) > MaxIssueCommentBodyBytes {
		t.Fatalf("anchor body exceeds GitHub cap: %d > %d", len(body), MaxIssueCommentBodyBytes)
	}
	if !strings.Contains(body, "Fishhawk run") {
		t.Errorf("header dropped by degradation ladder: header must survive")
	}
	if !strings.Contains(body, "Current plan summary stays.") {
		t.Errorf("current plan summary must survive the degradation ladder")
	}
	if !strings.Contains(body, "https://app.example/runs/11111111") {
		t.Errorf("dashboard deep-link must survive the degradation ladder")
	}
}

func TestRenderAnchorBody_NilRun(t *testing.T) {
	if got := RenderAnchorBody(AnchorInput{}); got != "" {
		t.Errorf("nil run should render empty; got %q", got)
	}
}
