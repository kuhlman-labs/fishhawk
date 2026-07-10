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
		}
		got := selectAnchorTimeline(entries, anchorTimelineLimit)
		if len(got) != 2 {
			t.Fatalf("got %d rows, want 2", len(got))
		}
		if got[0].Sequence != 2 || got[1].Sequence != 1 {
			t.Errorf("not most-recent-first: %d then %d", got[0].Sequence, got[1].Sequence)
		}
	})
	t.Run("over-cap keeps all retained, drops oldest informational", func(t *testing.T) {
		var entries []*audit.Entry
		// 4 retained (fix-ups at seq 1..4) + 10 informational (run_dispatched 5..14).
		for i := int64(1); i <= 4; i++ {
			entries = append(entries, activityEntry(i, "fixup_pushed", map[string]any{"files_changed_count": int(i)}))
		}
		for i := int64(5); i <= 14; i++ {
			entries = append(entries, activityEntry(i, "run_dispatched", nil))
		}
		got := selectAnchorTimeline(entries, anchorTimelineLimit)
		if len(got) != anchorTimelineLimit {
			t.Fatalf("got %d rows, want %d", len(got), anchorTimelineLimit)
		}
		retained := 0
		for _, e := range got {
			if e.Category == "fixup_pushed" {
				retained++
			}
		}
		if retained != 4 {
			t.Errorf("kept %d retained rows, want all 4", retained)
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
}

func TestRenderAnchorBody_NilRun(t *testing.T) {
	if got := RenderAnchorBody(AnchorInput{}); got != "" {
		t.Errorf("nil run should render empty; got %q", got)
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

// TestAssembleAnchor_EconomicsDroppedFirst pins the #1702 degradation ordering
// directly on the ladder: the economics block is the FIRST droppable section
// shed under the comment cap, before the timeline and superseded plans. The
// header, what-now line, current plan, and footer are never dropped.
func TestAssembleAnchor_EconomicsDroppedFirst(t *testing.T) {
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
		footer:          "FOOTER",
	}

	l0 := assembleAnchor(s, 0)
	for _, want := range []string{"ECONOMICS", "TIMELINE", "SUPERSEDED", "HEADER", "CURRENTPLAN", "FOOTER"} {
		if !strings.Contains(l0, want) {
			t.Errorf("level 0 must contain %q:\n%s", want, l0)
		}
	}

	// Level 1 sheds economics FIRST — timeline and superseded still present.
	l1 := assembleAnchor(s, 1)
	if strings.Contains(l1, "ECONOMICS") {
		t.Errorf("economics must be dropped at level 1 (first):\n%s", l1)
	}
	if !strings.Contains(l1, "TIMELINE") || !strings.Contains(l1, "SUPERSEDED") {
		t.Errorf("timeline and superseded must survive level 1 (dropped after economics):\n%s", l1)
	}

	// Level 2 sheds the timeline; superseded still present.
	l2 := assembleAnchor(s, 2)
	if strings.Contains(l2, "TIMELINE") {
		t.Errorf("timeline must be dropped at level 2:\n%s", l2)
	}
	if !strings.Contains(l2, "SUPERSEDED") {
		t.Errorf("superseded must survive level 2:\n%s", l2)
	}

	// Level 3 sheds superseded plans; the never-dropped sections remain.
	l3 := assembleAnchor(s, 3)
	if strings.Contains(l3, "SUPERSEDED") {
		t.Errorf("superseded must be dropped at level 3:\n%s", l3)
	}
	for _, want := range []string{"HEADER", "WHATNOW", "CURRENTPLAN", "FOOTER"} {
		if !strings.Contains(l3, want) {
			t.Errorf("level 3 must still contain the never-dropped section %q:\n%s", want, l3)
		}
	}
}
