package issuecomment

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
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

// TestRenderAnchorBody_UnsetExternalURL_DegradesLinks pins #1787 at the issue
// anchor locus: with the base URL unset the header renders the plain backticked
// short-id (no link) and the footer omits the "view run" link entirely (leaving
// no dangling middot before the PR link), and nothing leaks a localhost literal
// or a relative run link. With the base URL configured the absolute link
// returns.
func TestRenderAnchorBody_UnsetExternalURL_DegradesLinks(t *testing.T) {
	r := anchorRun()
	prURL := "https://github.com/kuhlman-labs/fishhawk/pull/7"
	r.PullRequestURL = &prURL
	in := AnchorInput{
		Run:    r,
		Stages: []*run.Stage{{Type: run.StageTypeImplement, State: run.StageStateRunning}},
		Now:    time.Unix(1000, 0).UTC(),
	}

	unset := RenderAnchorBody(in)
	if strings.Contains(unset, "localhost") || strings.Contains(unset, "/runs/") || strings.Contains(unset, "](/") {
		t.Errorf("unset anchor leaked a run link:\n%s", unset)
	}
	if !strings.Contains(unset, "**Fishhawk run `11111111`**") {
		t.Errorf("unset anchor header should carry the plain backticked short-id:\n%s", unset)
	}
	// The PR link survives (it is not derived from the base URL), and there is
	// no leading "· " before it (the omitted run link took no separator).
	if !strings.Contains(unset, "[Pull request →]("+prURL+")") {
		t.Errorf("unset anchor should still carry the PR link:\n%s", unset)
	}
	if strings.Contains(unset, "· [Pull request →]") {
		t.Errorf("unset anchor footer left a dangling middot before the PR link:\n%s", unset)
	}

	in.ExternalURL = "https://app.example"
	cfg := RenderAnchorBody(in)
	if !strings.Contains(cfg, "https://app.example/runs/"+r.ID.String()) {
		t.Errorf("configured anchor should carry the absolute run link:\n%s", cfg)
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

// TestRenderAnchorBody_AcceptanceRetirementDroppedTimeline pins #3392: a
// dropped acceptance-scenario retirement surfaces on the living anchor with
// every retired scenario id and reason, and (like every other anchor-
// timeline row) no @-mention.
func TestRenderAnchorBody_AcceptanceRetirementDroppedTimeline(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"reason": "persist_failed:push: rejected",
		"retired": []map[string]any{
			{"id": "scenario:issue-101/crit-b", "reason": "behaviour replaced"},
			{"id": "scenario:issue-101/crit-c", "reason": "superseded by crit-d"},
		},
	})
	entries := []*audit.Entry{
		{Sequence: 9, Category: "acceptance_scenario_retirement_dropped", Payload: payload, Timestamp: time.Unix(9, 0).UTC()},
	}
	body := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypeAcceptance, State: run.StageStateSucceeded}},
		Audit:       entries,
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	})
	for _, want := range []string{
		"`scenario:issue-101/crit-b`", "behaviour replaced",
		"`scenario:issue-101/crit-c`", "superseded by crit-d",
		"2 scenarios still replayed", "(persist_failed:push: rejected)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("anchor timeline missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "@") {
		t.Errorf("anchor timeline must never @-mention: %q", body)
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

// TestRenderAnchorBody_UnpublishedRevisionsNote pins the one-shot
// (!UpdateOnChange) render (E45.41 / #3346): a zero UnpublishedRevisions
// renders no note, and a positive count names it.
func TestRenderAnchorBody_UnpublishedRevisionsNote(t *testing.T) {
	base := AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateRunning}},
		ExternalURL: "https://app.example",
		Now:         time.Now(),
	}

	t.Run("zero revisions renders no note", func(t *testing.T) {
		in := base
		in.CurrentPlan = &AnchorPlanView{Summary: "Add the thing.", UnpublishedRevisions: 0}
		body := RenderAnchorBody(in)
		if strings.Contains(body, "does not set `update_on_change`") {
			t.Errorf("expected no unpublished-revisions note when UnpublishedRevisions is 0: %q", body)
		}
	})

	t.Run("two revisions renders the note naming the count", func(t *testing.T) {
		in := base
		in.CurrentPlan = &AnchorPlanView{Summary: "Add the thing.", UnpublishedRevisions: 2}
		body := RenderAnchorBody(in)
		if !strings.Contains(body, "revised 2 time(s)") {
			t.Errorf("expected unpublished-revisions note naming 2 time(s): %q", body)
		}
		if !strings.Contains(body, "does not set `update_on_change`") {
			t.Errorf("expected unpublished-revisions note explaining the cause: %q", body)
		}
	})
}

// TestRenderAnchorBody_PlanEchoSuppressed pins the !Declared render (E45.41
// / #3346): the anchor's plan section collapses to a one-line run-page
// pointer with no summary and no Plan details block, and the marker +
// deep-link invariants still hold in that shape. Also pins that the
// suppression fires even when CurrentPlan is nil (the loadAnchorPlans
// !Declared branch always returns a nil CurrentPlan).
func TestRenderAnchorBody_PlanEchoSuppressed(t *testing.T) {
	r := anchorRun()
	base := AnchorInput{
		Run:         r,
		Stages:      []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateRunning}},
		ExternalURL: "https://app.example",
		Now:         time.Now(),
	}

	t.Run("suppressed with nil CurrentPlan still renders the pointer line", func(t *testing.T) {
		in := base
		in.PlanEchoSuppressed = true
		body := RenderAnchorBody(in)
		if !strings.Contains(body, "Not echoed to this issue") {
			t.Errorf("expected the not-echoed pointer line: %q", body)
		}
		if !strings.Contains(body, "declares no `originating_issue` persistence") {
			t.Errorf("expected the pointer line to name the cause: %q", body)
		}
		if strings.Contains(body, "Plan details") {
			t.Errorf("suppressed anchor must not render a Plan details block: %q", body)
		}
		if !strings.Contains(body, stickyMarker(stickyLocusAnchor, r.ID)) {
			t.Errorf("marker invariant must hold in the suppressed shape: %q", body)
		}
		if !strings.Contains(body, "[View run →]") {
			t.Errorf("deep-link invariant must hold in the suppressed shape: %q", body)
		}
	})

	t.Run("suppressed WITH a populated CurrentPlan still shows only the pointer", func(t *testing.T) {
		in := base
		in.PlanEchoSuppressed = true
		in.CurrentPlan = &AnchorPlanView{Summary: "Should never render."}
		body := RenderAnchorBody(in)
		if strings.Contains(body, "Should never render.") {
			t.Errorf("suppression must win over a populated CurrentPlan: %q", body)
		}
		if !strings.Contains(body, "Not echoed to this issue") {
			t.Errorf("expected the not-echoed pointer line: %q", body)
		}
	})
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

// TestRenderAnchorBody_PerReviewerBlocks pins the refined per-reviewer-block
// shape (#1073 → #1788): a bare approve with no concerns and no free_form has
// nothing to expand, so it emits NO per-reviewer <details> (not a content-free
// "(no additional notes)" one) — while the inline summary line still lists
// every reviewer, so a two-reviewer round is never misread as one.
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
	if strings.Contains(body, "<details><summary>claude-opus-4-8: approve</summary>") {
		t.Errorf("a content-free approve must not emit a per-reviewer block: %q", body)
	}
	if strings.Contains(body, "<details><summary>gpt-5.5: approve</summary>") {
		t.Errorf("a content-free approve must not emit a per-reviewer block: %q", body)
	}
	if strings.Contains(body, "(no additional notes)") {
		t.Errorf("empty-details body must be dropped entirely: %q", body)
	}
	// The inline summary line still names every reviewer.
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

// activityEntry builds a recognized activity audit entry for the timeline
// curation tests, stamping a deterministic 1970-dated timestamp keyed to the
// sequence (so every rendered row carries a "· 1970-01-01 …" stamp that the
// tests count to assert the row cap).
func activityEntry(seq int64, category string, payload map[string]any) *audit.Entry {
	var raw json.RawMessage
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	return &audit.Entry{Sequence: seq, Category: category, Payload: raw, Timestamp: time.Unix(seq, 0).UTC()}
}

// countTimelineRows counts rendered timeline rows by the per-row absolute
// timestamp stamp (anchorTimestamp renders "1970-01-01 …Z" for the tests'
// Unix-epoch entries; no other anchor section stamps a timestamp), so the
// count is exactly the number of timeline rows the curation admitted.
func countTimelineRows(body string) int {
	return strings.Count(body, "· 1970-01-01")
}

// TestRenderAnchorBody_TimelineCuration is the E42.6 / #1789 done-means test:
// the anchor timeline is curated by event CLASS, not pure recency, so an
// eventful run's gate decisions, fix-up pushes, waives, defers, and
// scope-amendment decisions (with their reasons) are RETAINED under the 12-row
// cap while only informational rows (dispatch/start heartbeats + model_resolved)
// are dropped. One case per selection branch: under-cap (nothing dropped),
// over-cap (informational dropped first), retained-alone-overflow (oldest
// retained trimmed, no informational shown), and reason-surfaced.
func TestRenderAnchorBody_TimelineCuration(t *testing.T) {
	// retainedOverflow: 13 retained fix-up rows (distinguishable by count) +
	// 2 informational rows. Retained alone exceeds the cap, so the oldest
	// retained (count=1) is trimmed and NO informational row survives.
	var retainedOverflow []*audit.Entry
	for i := int64(1); i <= 13; i++ {
		retainedOverflow = append(retainedOverflow,
			activityEntry(i, "fixup_pushed", map[string]any{"files_changed_count": int(i)}))
	}
	retainedOverflow = append(retainedOverflow,
		activityEntry(14, "run_dispatched", nil),
		activityEntry(15, "run_dispatched", nil),
	)

	tests := []struct {
		name        string
		entries     []*audit.Entry
		wantContain []string
		wantAbsent  []string
		wantRows    int    // exact rendered timeline row count (0 = skip)
		orderFirst  string // must appear before orderSecond (0 = skip)
		orderSecond string
	}{
		{
			name: "over-cap drops informational rows first, retains every decision + terminal",
			entries: []*audit.Entry{
				activityEntry(1, "plan_generated", nil),                                        // retained
				activityEntry(2, "run_dispatched", nil),                                        // informational
				approvalEntry(t, 3, "alice", "approve", ""),                                    // retained gate decision
				activityEntry(4, "model_resolved", map[string]any{"model": "claude-opus-4-8"}), // informational
				activityEntry(5, "fixup_pushed", map[string]any{"files_changed_count": 2}),     // retained
				activityEntry(6, "acceptance_dispatched", nil),                                 // informational
				activityEntry(7, "concern_waived", map[string]any{"severity": "high", "category": "correctness", "reason": "acceptable in this slice"}),
				activityEntry(8, "concern_deferred", map[string]any{"issue_number": 1790, "reason": "tracked as follow-up"}),
				activityEntry(9, "fixup_pushed", map[string]any{"files_changed_count": 5}),      // retained
				activityEntry(10, "model_resolved", map[string]any{"model": "claude-opus-4-8"}), // informational
				activityEntry(11, "scope_amendment_decided", map[string]any{"decision": "approve", "reason": "coupled test sibling"}),
				activityEntry(12, "run_dispatched", nil),                                        // informational
				activityEntry(13, "acceptance_dispatched", nil),                                 // informational
				activityEntry(14, "model_resolved", map[string]any{"model": "claude-opus-4-8"}), // informational
				activityEntry(20, "pr_merged", map[string]any{}),                                // retained terminal
			},
			// All 8 retained rows survive, each decision carrying its reason.
			wantContain: []string{
				"Plan posted",
				"`alice` approved the plan",
				"Fix-up pushed (2 files changed)",
				"Fix-up pushed (5 files changed)",
				"Concern waived (high correctness): acceptable in this slice",
				"Concern deferred to #1790: tracked as follow-up",
				"Scope amendment approved: coupled test sibling",
				"merged the PR",
			},
			// 8 retained + 4 backfilled informational = exactly the 12-row cap; the
			// 3 oldest informational rows are the only rows dropped.
			wantRows:    anchorTimelineLimit,
			orderFirst:  "merged the PR", // seq 20, newest
			orderSecond: "Plan posted",   // seq 1, oldest — proves most-recent-first
		},
		{
			name: "under-cap keeps every recognized row unchanged",
			entries: []*audit.Entry{
				activityEntry(1, "run_dispatched", nil),
				activityEntry(2, "plan_generated", nil),
				approvalEntry(t, 3, "alice", "approve", ""),
				activityEntry(4, "fixup_pushed", map[string]any{"files_changed_count": 1}),
				activityEntry(5, "model_resolved", map[string]any{"model": "claude-opus-4-8"}),
			},
			wantContain: []string{
				"Fishhawk run dispatched",
				"Plan posted",
				"`alice` approved the plan",
				"Fix-up pushed (1 file changed)",
				"Implement model resolved",
			},
			wantRows: 5,
		},
		{
			name:    "retained-alone-overflow trims oldest retained, shows no informational",
			entries: retainedOverflow,
			wantContain: []string{
				"Fix-up pushed (13 files changed)", // newest retained kept
				"Fix-up pushed (2 files changed)",  // second-oldest retained kept
			},
			wantAbsent: []string{
				"Fix-up pushed (1 file changed)", // oldest retained trimmed by the cap
				"Fishhawk run dispatched",        // no informational row survives
			},
			wantRows: anchorTimelineLimit,
		},
		{
			name: "reasons surfaced for waive, defer, and rejected amendment",
			entries: []*audit.Entry{
				activityEntry(1, "concern_waived", map[string]any{"severity": "medium", "category": "style", "reason": "cosmetic only"}),
				activityEntry(2, "concern_deferred", map[string]any{"issue_number": 42, "reason": "separate PR"}),
				activityEntry(3, "scope_amendment_decided", map[string]any{"decision": "deny", "reason": "belongs elsewhere"}),
			},
			wantContain: []string{
				"Concern waived (medium style): cosmetic only",
				"Concern deferred to #42: separate PR",
				"Scope amendment rejected: belongs elsewhere",
			},
			wantRows: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := RenderAnchorBody(AnchorInput{
				Run:         anchorRun(),
				Stages:      []*run.Stage{{Type: run.StageTypeImplement, State: run.StageStateRunning}},
				Audit:       tt.entries,
				ExternalURL: "https://app.example",
				Now:         time.Unix(1000, 0).UTC(),
			})
			for _, want := range tt.wantContain {
				if !strings.Contains(body, want) {
					t.Errorf("timeline missing %q:\n%s", want, body)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("timeline should not contain %q:\n%s", absent, body)
				}
			}
			if tt.wantRows > 0 {
				if got := countTimelineRows(body); got != tt.wantRows {
					t.Errorf("timeline rendered %d rows, want %d (cap %d)\n%s", got, tt.wantRows, anchorTimelineLimit, body)
				}
			}
			if tt.orderFirst != "" && tt.orderSecond != "" {
				fi, si := strings.Index(body, tt.orderFirst), strings.Index(body, tt.orderSecond)
				if fi < 0 || si < 0 || fi >= si {
					t.Errorf("expected %q (idx %d) before %q (idx %d) — most-recent-first order broken\n%s",
						tt.orderFirst, fi, tt.orderSecond, si, body)
				}
			}
		})
	}
}

// TestSelectAnchorTimeline_Partition unit-tests the selection branches directly
// (independent of rendering): under-cap returns all rows, over-cap keeps every
// retained row and drops the oldest informational, retained-alone-overflow
// trims the oldest retained and admits no informational, and the result is
// always most-recent-first and bounded by the limit.
func TestSelectAnchorTimeline_Partition(t *testing.T) {
	t.Run("under-cap returns all recognized rows", func(t *testing.T) {
		entries := []*audit.Entry{
			activityEntry(1, "run_dispatched", nil),
			activityEntry(2, "fixup_pushed", map[string]any{"files_changed_count": 1}),
			activityEntry(3, "acceptance_scenario_retirement_dropped",
				map[string]any{"reason": "persist_failed", "retired": []map[string]any{{"id": "scenario:issue-1/a", "reason": "r"}}}),
		}
		got := selectAnchorTimeline(entries, anchorTimelineLimit)
		if len(got) != 3 {
			t.Fatalf("got %d rows, want 3", len(got))
		}
		if got[0].Sequence != 3 || got[1].Sequence != 2 || got[2].Sequence != 1 {
			t.Errorf("not most-recent-first: %d, %d, %d", got[0].Sequence, got[1].Sequence, got[2].Sequence)
		}
	})
	t.Run("over-cap keeps all retained, drops oldest informational", func(t *testing.T) {
		var entries []*audit.Entry
		// 5 retained (fix-ups at seq 1..4 + one retirement-drop at seq 5) + 10
		// informational (run_dispatched 6..15).
		for i := int64(1); i <= 4; i++ {
			entries = append(entries, activityEntry(i, "fixup_pushed", map[string]any{"files_changed_count": int(i)}))
		}
		entries = append(entries, activityEntry(5, "acceptance_scenario_retirement_dropped",
			map[string]any{"reason": "persist_failed", "retired": []map[string]any{{"id": "scenario:issue-1/a", "reason": "r"}}}))
		for i := int64(6); i <= 15; i++ {
			entries = append(entries, activityEntry(i, "run_dispatched", nil))
		}
		got := selectAnchorTimeline(entries, anchorTimelineLimit)
		if len(got) != anchorTimelineLimit {
			t.Fatalf("got %d rows, want %d", len(got), anchorTimelineLimit)
		}
		retained := 0
		sawRetirementDrop := false
		for _, e := range got {
			if e.Category == "fixup_pushed" {
				retained++
			}
			if e.Category == "acceptance_scenario_retirement_dropped" {
				sawRetirementDrop = true
			}
		}
		if retained != 4 {
			t.Errorf("kept %d retained fixup_pushed rows, want all 4", retained)
		}
		if !sawRetirementDrop {
			t.Errorf("acceptance_scenario_retirement_dropped was treated as informational and dropped under the cap")
		}
		// Most-recent-first + bounded.
		for i := 1; i < len(got); i++ {
			if got[i-1].Sequence < got[i].Sequence {
				t.Errorf("not most-recent-first at %d: %d then %d", i, got[i-1].Sequence, got[i].Sequence)
			}
		}
	})
	t.Run("retained-alone-overflow trims oldest retained, admits no informational", func(t *testing.T) {
		var entries []*audit.Entry
		for i := int64(1); i <= 14; i++ {
			entries = append(entries, activityEntry(i, "fixup_pushed", map[string]any{"files_changed_count": int(i)}))
		}
		entries = append(entries, activityEntry(15, "run_dispatched", nil))
		got := selectAnchorTimeline(entries, anchorTimelineLimit)
		if len(got) != anchorTimelineLimit {
			t.Fatalf("got %d rows, want %d", len(got), anchorTimelineLimit)
		}
		for _, e := range got {
			if e.Category == "run_dispatched" {
				t.Errorf("informational row leaked into a retained-overflow selection")
			}
			if e.Sequence <= 2 {
				t.Errorf("oldest retained (seq %d) should have been trimmed", e.Sequence)
			}
		}
	})
	t.Run("zero limit returns nil", func(t *testing.T) {
		if got := selectAnchorTimeline([]*audit.Entry{activityEntry(1, "fixup_pushed", nil)}, 0); got != nil {
			t.Errorf("limit 0 should return nil, got %v", got)
		}
	})
}

// TestRenderAnchorBody_GateDecisionTimeline covers the enriched
// gate-decision timeline entry (#1070): the decision phrase, the
// conditions <details>, and the "over N advisory reject(s)" arbitration
// marker — each only when the underlying chain warrants it.
func TestRenderAnchorBody_GateDecisionTimeline(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	tokenPayload, _ := json.Marshal(map[string]any{"decision": "approve", "approver": "brett@local-mcp"})
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
				"`alice` approved the plan with conditions",
				"<details><summary>Approval conditions</summary>",
				"keep the two-round test",
			},
			// The anchor timeline never @-mentions an actor (#751/#755/#1788).
			wantAbsent: []string{"advisory reject", "@alice"},
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
			wantContain: []string{"`alice` approved the plan (over 1 advisory reject)"},
			wantAbsent:  []string{"Approval conditions", "over 2 advisory", "@alice"},
		},
		{
			name: "clean approve",
			entries: []*audit.Entry{
				approvalEntry(t, 5, "alice", "approve", ""),
			},
			wantContain: []string{"`alice` approved the plan"},
			wantAbsent:  []string{"advisory reject", "Approval conditions", "with conditions", "@alice"},
		},
		{
			// A non-login token subject flows through renderApproverIdentity's
			// no-@ code-span form (the anchor never pings a real user, #755/#1788).
			name: "token subject approve renders no @-mention",
			entries: []*audit.Entry{
				{Sequence: 5, Category: "approval_submitted", Payload: tokenPayload, Timestamp: time.Unix(5, 0).UTC()},
			},
			// The subject renders inside a backtick code span (which itself
			// contains the literal @), so the guard is that it never appears as
			// a bare leading @-mention that GitHub would resolve to a user.
			wantContain: []string{"`brett@local-mcp` approved the plan"},
			wantAbsent:  []string{"@brett", " @`", "by @"},
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
	// The hidden sticky marker (#1793) is accounted for in the size budget and
	// preserved through the ladder + tail-truncation (it leads the body).
	if marker := stickyMarker(stickyLocusAnchor, anchorRun().ID); !strings.Contains(body, marker) {
		t.Errorf("sticky marker must survive the degradation ladder: want %q in body", marker)
	}
}

func TestRenderAnchorBody_NilRun(t *testing.T) {
	if got := RenderAnchorBody(AnchorInput{}); got != "" {
		t.Errorf("nil run should render empty; got %q", got)
	}
}

// TestRenderAnchorBody_EmbedsStickyMarker pins #1793: the living-anchor body
// leads with the hidden anchor sticky marker, and the CLI status-comment
// renderer (RenderStatusBody) embeds the IDENTICAL marker — both edit the same
// anchor comment, so a mismatch would strip the marker on a CLI edit and defeat
// orphan re-discovery.
func TestRenderAnchorBody_EmbedsStickyMarker(t *testing.T) {
	r := anchorRun()
	marker := stickyMarker(stickyLocusAnchor, r.ID)
	if marker != "<!-- fishhawk-sticky locus=anchor run=11111111-2222-3333-4444-555555555555 -->" {
		t.Fatalf("unexpected marker format: %q", marker)
	}
	anchor := RenderAnchorBody(AnchorInput{Run: r, ExternalURL: "https://app.example", Now: time.Now()})
	if !strings.HasPrefix(anchor, marker) {
		t.Errorf("anchor marker must be the FIRST body section; body = %q", anchor)
	}
	status := RenderStatusBody(r, nil, nil, "https://app.example", time.Now())
	if !strings.Contains(status, marker) {
		t.Errorf("status body missing IDENTICAL anchor marker %q; body = %q", marker, status)
	}
}

// TestRenderAnchorBody_AbsoluteTimestamps pins fix 1 (#1788): anchor timeline
// rows carry an absolute UTC stamp (`YYYY-MM-DD HH:MMZ`) that reads correctly
// once the run settles, NOT a relative "5m ago"/"just now" that freezes at the
// last render. It also pins fix 2: the row's actor renders as a backtick code
// span, never an @-mention.
func TestRenderAnchorBody_AbsoluteTimestamps(t *testing.T) {
	ts := time.Date(2026, 7, 9, 23, 36, 0, 0, time.UTC)
	alice := "alice"
	entries := []*audit.Entry{
		{Sequence: 5, Category: "pr_merged", ActorSubject: &alice, Timestamp: ts},
	}
	body := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypeImplement, State: run.StageStateRunning}},
		Audit:       entries,
		ExternalURL: "https://app.example",
		// Now is a full day later; a relative age would render "1d ago".
		Now: time.Date(2026, 7, 10, 23, 36, 0, 0, time.UTC),
	})
	if !strings.Contains(body, "2026-07-09 23:36Z") {
		t.Errorf("timeline row should carry an absolute UTC stamp:\n%s", body)
	}
	for _, rel := range []string{"m ago", "h ago", "d ago", "just now"} {
		if strings.Contains(body, rel) {
			t.Errorf("timeline must not carry a relative age %q:\n%s", rel, body)
		}
	}
	if strings.Contains(body, "@alice") {
		t.Errorf("timeline actor must render as a backtick code span, not an @-mention:\n%s", body)
	}
	if !strings.Contains(body, "`alice` merged the PR") {
		t.Errorf("timeline actor should render as a backtick code span:\n%s", body)
	}
}

// TestTruncateWords covers truncateWords' three branches directly: a string
// that already fits (returned unchanged, no ellipsis), a word-boundary cut
// (breaks on the last space with a real "…"), and the no-space fallback (a
// single over-long token backs off to a rune boundary rather than never
// truncating).
func TestTruncateWords(t *testing.T) {
	if got := truncateWords("short", 200); got != "short" {
		t.Errorf("a fitting string must be returned unchanged; got %q", got)
	}
	// Word boundary: "aaa bbb ccc ddd" capped at 9 → last space ≤9 is after
	// "bbb" (index 7), so "aaa bbb…".
	if got := truncateWords("aaa bbb ccc ddd", 9); got != "aaa bbb…" {
		t.Errorf("word-boundary cut = %q, want %q", got, "aaa bbb…")
	}
	// No space in range: a single long token backs off to a rune boundary and
	// still truncates (never returns the whole over-cap token).
	got := truncateWords("aaaaaaaaaaaaaaa", 5)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) > 6 {
		t.Errorf("no-space fallback should truncate with an ellipsis; got %q", got)
	}
}

// TestRenderAnchorBody_RationaleWordBoundaryTruncation pins fix 4a (#1788): an
// over-cap model-recommendation rationale truncates at a WORD boundary with a
// real "…" ellipsis, never mid-word and never the ASCII "...".
func TestRenderAnchorBody_RationaleWordBoundaryTruncation(t *testing.T) {
	// 40 × "complexity " is ~440 bytes — well over the 200-byte cap — and every
	// token is the same word, so a word-boundary cut ends on a whole
	// "complexity".
	rationale := strings.TrimSpace(strings.Repeat("complexity ", 40))
	body := RenderAnchorBody(AnchorInput{
		Run:    anchorRun(),
		Stages: []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateSucceeded}},
		CurrentPlan: &AnchorPlanView{
			Summary:                 "s",
			RecommendedModel:        "claude-sonnet-4-6",
			RecommendationRationale: rationale,
		},
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	})
	if !strings.Contains(body, "complexity…") {
		t.Errorf("rationale should truncate on a word boundary with a real ellipsis:\n%s", body)
	}
	// Isolate the recommendation line and assert it uses the real ellipsis, not
	// the ASCII "..." the shared oneLine/truncate would emit.
	line := body[strings.Index(body, "Model recommendation:"):]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	if strings.Contains(line, "...") {
		t.Errorf("rationale must use a real ellipsis, not ASCII '...':\n%s", line)
	}
}

// TestRenderAnchorBody_Economics pins #1702: a wired economics rollup renders
// the block (above the footer) in a normal-size anchor body.
func TestRenderAnchorBody_Economics(t *testing.T) {
	econ := fullEconomics()
	body := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypeReview, State: run.StageStateSucceeded}},
		Economics:   &econ,
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	})
	for _, want := range []string{
		"**Economics**",
		"**Total cost**: $0.42",
		"**Wait on human**: 1h 30m",
		"plan approval: 45m",
		"**Cache net savings**: $0.12 (vs uncached replay)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("anchor missing economics content %q:\n%s", want, body)
		}
	}
	// The block sits above the footer (dashboard deep-link).
	if econIdx, footIdx := strings.Index(body, "**Economics**"), strings.Index(body, "[View run →]"); econIdx < 0 || footIdx < 0 || econIdx > footIdx {
		t.Errorf("economics block must render above the footer (econ=%d foot=%d):\n%s", econIdx, footIdx, body)
	}
}

// TestRenderAnchorBody_EconomicsNilOmitted asserts a nil economics rollup
// omits the block entirely (graceful degradation — the anchor renders
// everything else).
func TestRenderAnchorBody_EconomicsNilOmitted(t *testing.T) {
	body := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypeReview, State: run.StageStateRunning}},
		Economics:   nil,
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	})
	if strings.Contains(body, "**Economics**") {
		t.Errorf("nil economics must omit the block:\n%s", body)
	}
}

// TestAssembleAnchor_LadderOrder pins the degradation ordering directly on the
// ladder: the gate precedent section (E75.4 / #3732) is the FIRST droppable
// section shed under the comment cap, and the pre-E75.4 relative order below it
// is preserved — economics (#1702), then the timeline, then superseded plans.
// The header, what-now line, current plan, and footer are never dropped.
func TestAssembleAnchor_LadderOrder(t *testing.T) {
	s := anchorSections{
		header:          "HEADER",
		whatNow:         "WHATNOW",
		stages:          "STAGES",
		timeline:        "TIMELINE",
		reviews:         "REVIEWS",
		currentPlan:     "CURRENTPLAN",
		modelResolved:   "MODEL",
		supersededPlans: "SUPERSEDED",
		economics:       "ECONOMICS",
		precedent:       "PRECEDENT",
		footer:          "FOOTER",
	}
	// gone[level] is the set of sections absent at that level; every other
	// droppable section must still be present.
	droppable := []string{"PRECEDENT", "ECONOMICS", "TIMELINE", "SUPERSEDED"}
	for level := 0; level <= anchorLadderFloor; level++ {
		body := assembleAnchor(s, level)
		for i, sec := range droppable {
			wantGone := i < level
			if gone := !strings.Contains(body, sec); gone != wantGone {
				t.Errorf("level %d: %s gone=%v, want %v:\n%s", level, sec, gone, wantGone, body)
			}
		}
		for _, want := range []string{"HEADER", "WHATNOW", "CURRENTPLAN", "FOOTER"} {
			if !strings.Contains(body, want) {
				t.Errorf("level %d must still contain the never-dropped section %q", level, want)
			}
		}
	}
	if anchorLadderDropPrecedent != 1 {
		t.Errorf("precedent drops at level %d, want 1 (FIRST)", anchorLadderDropPrecedent)
	}
}

// evidencedReviewedEntry builds a *_reviewed audit entry whose concerns carry
// the reviewer's new_evidence (#1913) on the wire. reviewedEntry above emits
// only severity/category/note, so a local builder is used rather than widening
// that shared helper's signature.
func evidencedReviewedEntry(t *testing.T, seq int64, stageType string, concerns []anchorReviewConcern) *audit.Entry {
	t.Helper()
	cs := make([]map[string]string, 0, len(concerns))
	for _, c := range concerns {
		cs = append(cs, map[string]string{
			"severity": c.severity, "category": c.category, "note": c.note, "new_evidence": c.evidence,
		})
	}
	payload, _ := json.Marshal(map[string]any{
		"reviewer_model": "gpt-5.6-sol",
		"verdict":        "reject",
		"concerns":       cs,
	})
	return &audit.Entry{Sequence: seq, Category: stageType + "_reviewed", Payload: payload, Timestamp: time.Unix(seq, 0).UTC()}
}

func evidenceAnchorBody(t *testing.T, concerns []anchorReviewConcern) string {
	t.Helper()
	return RenderAnchorBody(AnchorInput{
		Run:    anchorRun(),
		Stages: []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}},
		Audit: []*audit.Entry{
			startedEntry(10, "plan"),
			evidencedReviewedEntry(t, 11, "plan", concerns),
		},
		ExternalURL: "https://app.example",
		Now:         time.Now(),
	})
}

// TestRenderAnchorBody_ConcernEvidenceSubLine is the issue-anchor leg of #2353:
// the sticky issue comment's per-reviewer verdict block rendered `note` alone,
// so the same well-evidenced rejection read as contentless there too.
func TestRenderAnchorBody_ConcernEvidenceSubLine(t *testing.T) {
	const evidence = "backend/internal/issuecomment/anchor_template.go:500 formats severity/category/note and nothing else"
	body := evidenceAnchorBody(t, []anchorReviewConcern{
		{severity: "high", category: "correctness", note: "boom", evidence: evidence},
	})
	if !strings.Contains(body, "- **high** (correctness): boom\n  - evidence: "+evidence+"\n") {
		t.Errorf("anchor missing the indented evidence sub-line under the concern:\n%s", body)
	}
}

// TestRenderAnchorBody_OmitsBlankConcernEvidence pins the suppression: a
// concern with no evidence (or whitespace-only evidence) must render NOTHING —
// never a dangling "evidence:" label, which reads as "the reviewer supplied no
// evidence" when the truth is that this concern never had any.
func TestRenderAnchorBody_OmitsBlankConcernEvidence(t *testing.T) {
	for name, evidence := range map[string]string{"empty": "", "whitespace_only": "  \n\t "} {
		t.Run(name, func(t *testing.T) {
			body := evidenceAnchorBody(t, []anchorReviewConcern{
				{severity: "high", category: "correctness", note: "boom", evidence: evidence},
			})
			if !strings.Contains(body, "- **high** (correctness): boom") {
				t.Fatalf("concern line missing entirely:\n%s", body)
			}
			if strings.Contains(body, "evidence:") {
				t.Errorf("blank evidence rendered a labelled line:\n%s", body)
			}
		})
	}
}

// TestRenderAnchorBody_ConcernEvidenceCollapsesNewlines pins the one shaping
// rule: multi-line evidence is whitespace-collapsed so it stays inside the
// markdown list item instead of breaking out of it. Nothing is truncated.
func TestRenderAnchorBody_ConcernEvidenceCollapsesNewlines(t *testing.T) {
	body := evidenceAnchorBody(t, []anchorReviewConcern{
		{severity: "high", category: "correctness", note: "boom", evidence: "line one\nline two\n\nline three"},
	})
	if !strings.Contains(body, "  - evidence: line one line two line three\n") {
		t.Errorf("multi-line evidence not collapsed onto one list line:\n%s", body)
	}
}

// captainApproval builds an approval_submitted entry carrying an explicit
// approver subject (and optional resolved login / delegated rule), the shape
// approvals.go writes.
func captainApproval(t *testing.T, seq int64, approver, login, delegated, decision string) *audit.Entry {
	t.Helper()
	payload := map[string]any{"decision": decision, "approver": approver}
	if login != "" {
		payload["approver_github_login"] = login
	}
	if delegated != "" {
		payload["delegated"] = delegated
	}
	raw, _ := json.Marshal(payload)
	return &audit.Entry{Sequence: seq, Category: "approval_submitted", Payload: raw, Timestamp: time.Unix(seq, 0).UTC()}
}

func seated(subject string) *CaptainResolution {
	return &CaptainResolution{Subject: subject, IdentityVerified: true, Basis: CaptainBasisCaptain}
}

// TestRenderAnchorBody_CaptainNilAndUnavailableAreByteIdentical pins the
// degrade contract (E76.3 / #3766): an unreadable captain record renders the
// anchor EXACTLY as the no-resolver (pre-E76.3) anchor — no vacancy line, no
// approval note — so an unreadable record is never asserted as a vacancy.
func TestRenderAnchorBody_CaptainNilAndUnavailableAreByteIdentical(t *testing.T) {
	entries := []*audit.Entry{captainApproval(t, 5, "github:bob", "bob", "", "approve")}
	base := AnchorInput{Run: anchorRun(), Audit: entries, Now: time.Unix(100, 0)}
	want := RenderAnchorBody(base)
	withUnavailable := base
	withUnavailable.Captain = &CaptainResolution{Basis: CaptainBasisUnavailable}
	if got := RenderAnchorBody(withUnavailable); got != want {
		t.Errorf("unavailable captain changed the anchor:\n--- got\n%s\n--- want\n%s", got, want)
	}
	if strings.Contains(want, "captain") {
		t.Errorf("nil-captain anchor must carry no captain text:\n%s", want)
	}
}

// TestRenderAnchorBody_VacantSeatStatesVacancy: a vacant seat is STATED
// under the header (ADR-083 rule 5), never implied.
func TestRenderAnchorBody_VacantSeatStatesVacancy(t *testing.T) {
	body := RenderAnchorBody(AnchorInput{
		Run: anchorRun(), Now: time.Unix(100, 0),
		Captain: &CaptainResolution{Basis: CaptainBasisVacant},
	})
	header := renderAnchorHeader(anchorRun(), "")
	if !strings.Contains(body, header+"\n\n"+captainVacantAnchorLine+"\n") {
		t.Errorf("vacancy line must follow the header:\n%s", body)
	}
}

// TestRenderAnchorBody_NonCaptainApprovalNote pins ADR-083 rule 4 on the
// anchor: an approval by a human other than the seated captain gains
// "; captain is `Y`", rendered as a code span (never a mention — the anchor
// is re-edited and must never re-ping). Each no-note arm isolates one guard:
// the approver IS the captain (by subject, and by resolved login), the
// approval is delegated/agent, the decision is a reject, or no captain sits.
func TestRenderAnchorBody_NonCaptainApprovalNote(t *testing.T) {
	const note = "; captain is `github:alice`"
	cases := []struct {
		name     string
		entry    *audit.Entry
		captain  *CaptainResolution
		wantNote bool
	}{
		{"different human approver", captainApproval(t, 5, "github:bob", "bob", "", "approve"), seated("github:alice"), true},
		{"static-subject approver", captainApproval(t, 5, "brett@local-mcp", "", "", "approve"), seated("github:alice"), true},
		{"approver is the captain by subject", captainApproval(t, 5, "github:alice", "", "", "approve"), seated("github:alice"), false},
		{"approver is the captain by resolved login", captainApproval(t, 5, "brett@local-mcp", "alice", "", "approve"), seated("github:alice"), false},
		{"delegated approval", captainApproval(t, 5, "github:bob", "", "gate_x", "approve"), seated("github:alice"), false},
		{"operator-agent token approval without a rule", captainApproval(t, 5, operatorrole.TokenSubjectPrefix+"v1", "", "", "approve"), seated("github:alice"), false},
		{"reject decision", captainApproval(t, 5, "github:bob", "bob", "", "reject"), seated("github:alice"), false},
		{"vacant seat", captainApproval(t, 5, "github:bob", "bob", "", "approve"), &CaptainResolution{Basis: CaptainBasisVacant}, false},
		// A non-captain basis carrying a stray subject isolates the basis
		// check from the empty-subject check.
		{"unavailable basis with a stray subject", captainApproval(t, 5, "github:bob", "bob", "", "approve"), &CaptainResolution{Subject: "github:alice", Basis: CaptainBasisUnavailable}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := RenderAnchorBody(AnchorInput{
				Run: anchorRun(), Audit: []*audit.Entry{tc.entry}, Now: time.Unix(100, 0), Captain: tc.captain,
			})
			if got := strings.Contains(body, note); got != tc.wantNote {
				t.Errorf("note present = %v, want %v\n%s", got, tc.wantNote, body)
			}
			if strings.Contains(body, "@alice") {
				t.Errorf("the anchor must never @-mention the captain:\n%s", body)
			}
		})
	}
}

// TestRenderAnchorBody_NonCaptainNoteSanitizesCaptain: a captain subject
// carrying a backtick cannot close the code span.
func TestRenderAnchorBody_NonCaptainNoteSanitizesCaptain(t *testing.T) {
	body := RenderAnchorBody(AnchorInput{
		Run: anchorRun(), Now: time.Unix(100, 0),
		Audit:   []*audit.Entry{captainApproval(t, 5, "github:bob", "bob", "", "approve")},
		Captain: &CaptainResolution{Subject: "evil`@x", Basis: CaptainBasisCaptain},
	})
	if !strings.Contains(body, "; captain is `evil'@x`") {
		t.Errorf("captain must render sanitized inside one code span:\n%s", body)
	}
}

// --- E75.4 / #3732: the gate precedent section ------------------------------

func anchorPrecedentFixture() *AnchorPrecedent {
	return &AnchorPrecedent{
		DecisionClass: "plan_approval", IndexVersion: "precedent-rank-v1",
		Count: 3, ModalOutcome: "approved", AgreementRatio: 2.0 / 3.0,
		Cited: []AnchorPrecedentCitation{
			{SourceSequence: 41, Outcome: "approved", ScoreTotal: 0.75},
			{SourceSequence: 17, Outcome: "", ScoreTotal: 0.5},
		},
	}
}

// TestRenderPrecedentSection renders the heading, the display-only framing,
// the aggregate, one citation line per cited decision, and the full-query
// pointer; nil and a block citing nothing render nothing.
func TestRenderPrecedentSection(t *testing.T) {
	got := RenderPrecedentSection(anchorPrecedentFixture())
	for _, want := range []string{
		AnchorPrecedentHeading,
		"display-only — never authority; the decision is still the captain's",
		"Most common outcome: `approved` (67% of 3).",
		AnchorPrecedentCitationMarker + "41` — `approved`, score 0.75",
		AnchorPrecedentCitationMarker + "17` — `unrecorded`, score 0.50",
		"`fishhawk_precedent`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("precedent section missing %q:\n%s", want, got)
		}
	}
	if RenderPrecedentSection(nil) != "" || RenderPrecedentSection(&AnchorPrecedent{DecisionClass: "x"}) != "" {
		t.Errorf("nil / empty-cited precedent must render nothing")
	}
}

// TestRenderAnchorBody_PrecedentPlacement: a wired precedent renders inside the
// anchor above the footer; nil renders the anchor exactly as before.
func TestRenderAnchorBody_PrecedentPlacement(t *testing.T) {
	base := AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}},
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	}
	without := RenderAnchorBody(base)
	base.Precedent = anchorPrecedentFixture()
	with := RenderAnchorBody(base)
	pIdx, fIdx := strings.Index(with, AnchorPrecedentHeading), strings.Index(with, "[View run →]")
	if pIdx < 0 || fIdx < 0 || pIdx > fIdx {
		t.Errorf("precedent section must render above the footer (precedent=%d footer=%d):\n%s", pIdx, fIdx, with)
	}
	if strings.Contains(without, AnchorPrecedentHeading) {
		t.Errorf("nil precedent must render no section:\n%s", without)
	}
	if strings.Replace(with, RenderPrecedentSection(base.Precedent)+"\n", "", 1) != without {
		t.Errorf("a wired precedent changed more than its own section:\n with    %q\n without %q", with, without)
	}
}

// ladderOverflowInput returns an anchor input carrying BOTH the economics block
// and the precedent section whose FULL body exceeds the comment cap by exactly
// overflow bytes, tuned through the current plan's scope path length.
func ladderOverflowInput(t *testing.T, overflow int) AnchorInput {
	t.Helper()
	econ := fullEconomics()
	in := AnchorInput{
		Run:         anchorRun(),
		Stages:      []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}},
		Audit:       []*audit.Entry{startedEntry(10, "plan")},
		Economics:   &econ,
		Precedent:   anchorPrecedentFixture(),
		ExternalURL: "https://app.example",
		Now:         time.Unix(1000, 0).UTC(),
	}
	build := func(n int) AnchorInput {
		c := in
		c.CurrentPlan = &AnchorPlanView{
			Summary: "Current plan summary stays.",
			Files:   []plan.ScopeFile{{Path: strings.Repeat("p", n), Operation: "modify"}},
		}
		c.SupersededPlans = []AnchorPlanView{{Summary: "Old plan", Files: []plan.ScopeFile{{Path: "old.go", Operation: "modify"}}}}
		return c
	}
	sectionsFor := func(c AnchorInput) anchorSections {
		return anchorSections{
			marker: stickyMarker(stickyLocusAnchor, c.Run.ID), header: renderAnchorHeader(c.Run, c.ExternalURL),
			captain: renderAnchorCaptain(c.Captain), whatNow: renderWhatNow(c.Run, c.Stages),
			stages: renderAnchorStages(c.Stages), timeline: renderAnchorTimeline(c.Audit, c.Captain),
			reviews: renderAnchorReviews(c.Stages, c.Audit), currentPlan: renderCurrentPlan(c.CurrentPlan, false),
			modelResolved: renderResolvedModel(c.Audit), supersededPlans: renderSupersededPlans(c.SupersededPlans),
			economics: renderEconomicsSection(c.Economics), precedent: RenderPrecedentSection(c.Precedent),
			footer: renderAnchorFooter(c.Run, c.ExternalURL),
		}
	}
	probe := len(assembleAnchor(sectionsFor(build(0)), 0))
	c := build(MaxIssueCommentBodyBytes + overflow - probe)
	if got := len(assembleAnchor(sectionsFor(c), 0)); got != MaxIssueCommentBodyBytes+overflow {
		t.Fatalf("fixture: full body = %d bytes, want cap+%d = %d", got, overflow, MaxIssueCommentBodyBytes+overflow)
	}
	return c
}

// TestAnchorLadder_DropsPrecedentFirst: a body over the cap by FEWER bytes than
// EITHER the precedent section or the economics block can be fixed by shedding
// exactly one of them, so the outcome discriminates the ladder's first position:
// precedent is gone while economics, the timeline and superseded plans all
// SURVIVE. The pre-E75.4 ladder (economics first) would keep precedent and drop
// economics here.
func TestAnchorLadder_DropsPrecedentFirst(t *testing.T) {
	const overflow = 16
	in := ladderOverflowInput(t, overflow)
	if n := len(RenderPrecedentSection(in.Precedent)); n <= overflow {
		t.Fatalf("fixture: precedent section %d bytes must exceed the overflow %d", n, overflow)
	}
	if n := len(renderEconomicsSection(in.Economics)); n <= overflow {
		t.Fatalf("fixture: economics block %d bytes must exceed the overflow %d", n, overflow)
	}
	body := RenderAnchorBody(in)
	if len(body) > MaxIssueCommentBodyBytes {
		t.Fatalf("body %d exceeds the cap", len(body))
	}
	if strings.Contains(body, AnchorPrecedentHeading) {
		t.Errorf("precedent section must be shed FIRST, but it survived")
	}
	for _, want := range []string{"**Economics**", "Old plan", "Current plan summary stays."} {
		if !strings.Contains(body, want) {
			t.Errorf("%q must survive when shedding precedent alone fits the cap", want)
		}
	}
}

// TestAnchorLadder_HarderCapKeepsOrderBelowPrecedent: a body over the cap by
// MORE than precedent + economics but less than precedent + economics + the
// timeline sheds precedent and economics and keeps the timeline and superseded
// plans — the preserved economics -> timeline -> superseded order.
func TestAnchorLadder_HarderCapKeepsOrderBelowPrecedent(t *testing.T) {
	probe := ladderOverflowInput(t, 0)
	pre := len(RenderPrecedentSection(probe.Precedent)) + 1
	econ := len(renderEconomicsSection(probe.Economics)) + 1
	in := ladderOverflowInput(t, pre+econ-2)
	body := RenderAnchorBody(in)
	if strings.Contains(body, AnchorPrecedentHeading) || strings.Contains(body, "**Economics**") {
		t.Errorf("precedent and economics must both be shed at this cap")
	}
	if !strings.Contains(body, "Old plan") {
		t.Errorf("superseded plans must survive (dropped after the timeline)")
	}
}

// TestRenderCurrentPlan_NewArchitecturalDecision pins the E78.4 / #3748 anchor
// line: a declared new_architectural_decision renders ONE italic line under the
// model recommendation (and above Plan details) naming the decision summary, the
// rationale and the related ADRs, with planner-authored newlines flattened so
// the line cannot break out of its italic span.
func TestRenderCurrentPlan_NewArchitecturalDecision(t *testing.T) {
	view := &AnchorPlanView{
		Summary:                 "Route audit reads through a tenant-scoped repository.",
		Files:                   []plan.ScopeFile{{Path: "backend/internal/audit/repo.go", Operation: "modify"}},
		RecommendedModel:        "claude-sonnet-4-6",
		RecommendationRationale: "medium complexity",
		ArchitecturalDecision: &plan.NewArchitecturalDecision{
			Rationale:       "introduces a per-tenant\ntrust boundary\r\non the audit read path",
			RelatedADRs:     []string{"ADR-057", "#3728\n"},
			DecisionSummary: "audit reads resolve\nthrough a tenant-scoped repository",
		},
	}
	got := renderCurrentPlan(view, false)
	const wantLine = "_New architectural decision: audit reads resolve through a tenant-scoped repository — " +
		"introduces a per-tenant trust boundary on the audit read path (related ADRs: ADR-057, #3728). " +
		"The captain decides whether it needs an ADR._"
	if !strings.Contains(got, "\n"+wantLine+"\n") {
		t.Fatalf("anchor plan missing the flattened new-architectural-decision line %q:\n%s", wantLine, got)
	}
	rec := strings.Index(got, "_Model recommendation:")
	line := strings.Index(got, "_New architectural decision:")
	details := strings.Index(got, "<details><summary>Plan details</summary>")
	if rec < 0 || details < 0 || rec >= line || line >= details {
		t.Errorf("new-architectural-decision line must sit after the model recommendation and before Plan details (rec=%d line=%d details=%d):\n%s", rec, line, details, got)
	}
}

// TestRenderCurrentPlan_NewArchitecturalDecision_NoADRsSaysNoneCited pins the
// empty related_adrs rendering: no existing ADR covers the direction, so the
// line says "none cited" rather than an empty list.
func TestRenderCurrentPlan_NewArchitecturalDecision_NoADRsSaysNoneCited(t *testing.T) {
	got := renderCurrentPlan(&AnchorPlanView{
		Summary: "s",
		ArchitecturalDecision: &plan.NewArchitecturalDecision{
			Rationale:       "first persistence shape for crew messages",
			RelatedADRs:     []string{},
			DecisionSummary: "crew messages persist in their own table",
		},
	}, false)
	if !strings.Contains(got, "(related ADRs: none cited).") {
		t.Errorf("empty related_adrs should render as none cited:\n%s", got)
	}
}

// TestRenderCurrentPlan_NewArchitecturalDecision_BoundsRationale pins that an
// over-long planner rationale is truncated on a word boundary (the
// RecommendationRationale treatment), so the anchor line stays bounded.
func TestRenderCurrentPlan_NewArchitecturalDecision_BoundsRationale(t *testing.T) {
	got := renderCurrentPlan(&AnchorPlanView{
		Summary: "s",
		ArchitecturalDecision: &plan.NewArchitecturalDecision{
			Rationale:       strings.TrimSpace(strings.Repeat("boundary ", 60)),
			RelatedADRs:     []string{"ADR-082"},
			DecisionSummary: "one line",
		},
	}, false)
	start := strings.Index(got, "_New architectural decision:")
	if start < 0 {
		t.Fatalf("missing new-architectural-decision line:\n%s", got)
	}
	line := got[start:]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	if !strings.Contains(line, "boundary…") {
		t.Errorf("over-long rationale should truncate on a word boundary with a real ellipsis:\n%s", line)
	}
	if strings.Count(line, "boundary") > 30 {
		t.Errorf("rationale not bounded: %d repetitions survived in %q", strings.Count(line, "boundary"), line)
	}
}

// TestRenderCurrentPlan_NewArchitecturalDecision_UndeclaredIsByteIdentical pins
// the additive guarantee and the Declared() guard: a view without the field, or
// with a MALFORMED one (whitespace-only rationale, summary or ADR id — values
// the schema's minLength:1 admits), renders byte-identically to the view with no
// field at all.
func TestRenderCurrentPlan_NewArchitecturalDecision_UndeclaredIsByteIdentical(t *testing.T) {
	base := AnchorPlanView{
		Summary:                 "Resolve the implement model at the gate.",
		Files:                   []plan.ScopeFile{{Path: "a.go", Operation: "modify"}},
		RecommendedModel:        "claude-sonnet-4-6",
		RecommendationRationale: "medium complexity",
	}
	want := renderCurrentPlan(&base, false)
	for _, tc := range []struct {
		name string
		d    *plan.NewArchitecturalDecision
	}{
		{"nil", nil},
		{"whitespace rationale", &plan.NewArchitecturalDecision{Rationale: "   ", RelatedADRs: []string{"ADR-082"}, DecisionSummary: "summary"}},
		{"whitespace summary", &plan.NewArchitecturalDecision{Rationale: "why", RelatedADRs: []string{"ADR-082"}, DecisionSummary: " \n "}},
		{"whitespace ADR id", &plan.NewArchitecturalDecision{Rationale: "why", RelatedADRs: []string{"ADR-082", "  "}, DecisionSummary: "summary"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := base
			v.ArchitecturalDecision = tc.d
			if got := renderCurrentPlan(&v, false); got != want {
				t.Errorf("undeclared new_architectural_decision changed the render:\n got: %q\nwant: %q", got, want)
			}
		})
	}
}
