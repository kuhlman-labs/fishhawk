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
	if !strings.Contains(body, "<details><summary>📋 Plan — Add the living anchor comment.</summary>") {
		t.Errorf("current plan should be a collapsed details with summary visible: %q", body)
	}
	if !strings.Contains(body, "`a.go`") {
		t.Errorf("current plan scope file should render: %q", body)
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
