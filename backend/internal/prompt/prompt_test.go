package prompt

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// fixturePlan returns a standard_v1 plan with all sections populated
// so the assertions can target the renderer's full output. Test-only.
func fixturePlan() *plan.Plan {
	return &plan.Plan{
		PlanVersion: "standard_v1",
		TicketReference: plan.TicketReference{
			Type: plan.TicketTypeGitHubIssue,
			URL:  "https://github.com/kuhlman-labs/example/issues/42",
			ID:   "kuhlman-labs/example#42",
		},
		GeneratedBy: plan.GeneratedBy{
			Agent:     "claude-code",
			Model:     "claude-opus-4-7",
			Timestamp: time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		},
		Summary: "Add a foo helper to pkg/bar.",
		Scope: plan.Scope{
			Files: []plan.ScopeFile{
				{Path: "pkg/bar/foo.go", Operation: plan.FileOpCreate},
				{Path: "pkg/bar/bar.go", Operation: plan.FileOpModify},
				{Path: "pkg/bar/legacy.go", Operation: plan.FileOpDelete},
			},
		},
		Approach: []plan.ApproachStep{
			{Step: 1, Description: "Define Foo on the bar.Service interface."},
			{Step: 2, Description: "Implement Foo with a table-driven test."},
		},
		Verification: plan.Verification{
			TestStrategy: "Unit tests in pkg/bar; existing integration suite covers downstream callers.",
			RollbackPlan: "Revert the PR; no data migrations.",
		},
		RisksAndAssumptions: []string{
			"Assumes bar.Service is the only foo consumer.",
		},
	}
}

func TestBuild_Implement_FullContext(t *testing.T) {
	got, err := Build("implement", Trigger{
		Source:      "github_issue",
		IssueNumber: 42,
		IssueTitle:  "Add foo",
		IssueBody:   "We need a foo function in pkg/bar.",
		IssueURL:    "https://github.com/kuhlman-labs/example/issues/42",
		Repo:        "kuhlman-labs/example",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"`kuhlman-labs/example`",
		// Implement-stage prompt links the issue (#244): number,
		// title, and URL appear, but body is dropped — the agent
		// fetches if it needs detail.
		"Triggering issue: #42 · Add foo",
		"URL: https://github.com/kuhlman-labs/example/issues/42",
		"Fetch the issue body via your GitHub tooling",
		"smallest set of changes",
		// PR description guidance + the path the runner reads (#206).
		PullRequestDescriptionPath,
		// PR body section structure (matches CLAUDE.md's hand-written
		// PR convention). Without these the agent tends to write the
		// summary as floating prose and only head up the Test plan
		// section, producing an orphan-prose-then-H2 layout.
		"## Summary",
		"## Test plan",
		"## Notes",
		"`- [ ] …`",
		// `Closes #N` instruction is conditional on a non-zero issue
		// number — without it the merge wouldn't auto-close the
		// originating issue.
		"Closes #42",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("prompt missing %q\n---\n%s", w, got)
		}
	}
	// The body should NOT be in the implement-stage prompt — that's
	// the whole point of #244. The plan-stage prompt still gets the
	// body (TestBuild_Plan covers that contract).
	if strings.Contains(got, "We need a foo function in pkg/bar.") {
		t.Errorf("implement prompt should not include the issue body verbatim:\n%s", got)
	}
}

func TestBuild_Implement_NoIssueRef_OmitsClosesGuidance(t *testing.T) {
	// Manual / non-issue-triggered runs have IssueNumber == 0;
	// `Closes #N` is meaningless and the prompt should not include
	// it. The PR-description path guidance still applies.
	got, err := Build("implement", Trigger{Repo: "x/y"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "Closes #") {
		t.Errorf("prompt should not mention 'Closes #' when IssueNumber is 0:\n%s", got)
	}
	if !strings.Contains(got, PullRequestDescriptionPath) {
		t.Errorf("prompt missing PR description path even without issue context:\n%s", got)
	}
}

func TestBuild_Implement_EmptyContext(t *testing.T) {
	got, err := Build("implement", Trigger{Repo: "x/y"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "no issue context provided") {
		t.Errorf("expected 'no issue context provided' fallback, got:\n%s", got)
	}
}

func TestBuild_Implement_BodyDropped(t *testing.T) {
	// #244: the implement-stage prompt links the issue but does
	// NOT render the body verbatim. A trigger with only a body
	// (no title, no URL) should fall through to the empty-context
	// branch — the body alone isn't enough to render a useful
	// link block.
	got, err := Build("implement", Trigger{
		IssueBody: "Just a description.",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "Just a description.") {
		t.Errorf("implement prompt should never render the issue body:\n%s", got)
	}
	if !strings.Contains(got, "no issue context provided") {
		t.Errorf("body-only trigger should fall through to empty-context branch:\n%s", got)
	}
}

func TestBuild_Plan(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		IssueTitle:  "Plan a refactor",
		Repo:        "x/y",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"implementation plan",
		"Do not modify source files",
		"Triggering issue: #7",
		PlanArtifactPath,
		"standard_v1",
		"scripts/sync-schemas",
		"docs/spec/",
		"citation",
		// #627: cross-boundary test directive — pin the greppable anchors.
		"spans multiple architectural layers",
		"integration/end-to-end test",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan prompt missing %q:\n%s", w, got)
		}
	}
}

func TestBuild_UnsupportedStage(t *testing.T) {
	_, err := Build("review", Trigger{IssueTitle: "anything"})
	if !errors.Is(err, ErrUnsupportedStage) {
		t.Errorf("expected ErrUnsupportedStage, got %v", err)
	}
	if !strings.Contains(err.Error(), `"review"`) {
		t.Errorf("error should name the stage type, got %v", err)
	}
}

func TestBuild_UnknownStage(t *testing.T) {
	_, err := Build("nonsense", Trigger{})
	if !errors.Is(err, ErrUnsupportedStage) {
		t.Errorf("expected ErrUnsupportedStage, got %v", err)
	}
}

func TestBuild_NoRepo(t *testing.T) {
	got, err := Build("implement", Trigger{IssueTitle: "x"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "this repository") {
		t.Errorf("expected 'this repository' fallback when Repo empty, got:\n%s", got)
	}
	if strings.Contains(got, "``") {
		t.Errorf("empty backtick block leaked into prompt:\n%s", got)
	}
}

func TestBuild_DeterministicOutput(t *testing.T) {
	tr := Trigger{
		Source:      "github_issue",
		IssueNumber: 42,
		IssueTitle:  "T",
		IssueBody:   "B",
		Repo:        "o/r",
	}
	a, _ := Build("implement", tr)
	b, _ := Build("implement", tr)
	if a != b {
		t.Errorf("Build is non-deterministic across calls:\nA: %s\nB: %s", a, b)
	}
}

func TestBuild_Implement_WithApprovedPlan_LeadsWithPlan(t *testing.T) {
	// Plan-as-contract (#223): when the implement-stage prompt is
	// built with an approved plan, the plan is the binding
	// instruction and the issue is background context. Assert all
	// the load-bearing pieces of the new framing land.
	got, err := Build("implement", Trigger{
		Source:       "github_issue",
		IssueNumber:  42,
		IssueTitle:   "Add foo",
		IssueBody:    "We need a foo helper.",
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	wants := []string{
		// Plan-section header is the new lead.
		"Approved plan (binding instruction)",
		// Plan content renders as readable prose, not JSON.
		"Add a foo helper to pkg/bar.",
		"pkg/bar/foo.go (create)",
		"pkg/bar/bar.go (modify)",
		"pkg/bar/legacy.go (delete)",
		"1. Define Foo on the bar.Service interface.",
		"2. Implement Foo with a table-driven test.",
		"Test strategy:",
		"Rollback plan:",
		"Risks & assumptions:",
		"Assumes bar.Service is the only foo consumer.",
		// Issue link (#244): number + title + URL only — no body.
		"Originating issue (link only — fetch if you need detail):",
		"Triggering issue: #42 · Add foo",
		// Adherence + divergence + staleness instructions.
		"binding instruction",
		"diverging silently",
		"materially changed since the plan was approved",
		// Existing PR-description instructions still present —
		// the plan addition is additive, not replacement.
		PullRequestDescriptionPath,
		"## Summary",
		"## Test plan",
		"Closes #42",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("prompt missing %q\n---\n%s", w, got)
		}
	}

	// Issue body must NOT appear in the implement-stage prompt
	// (#244): linking is the new contract.
	if strings.Contains(got, "We need a foo helper.") {
		t.Errorf("implement prompt should not include the issue body verbatim:\n%s", got)
	}

	// The plan must come BEFORE the issue link in the prompt —
	// the lead-with-plan framing is the whole point.
	planIdx := strings.Index(got, "Approved plan (binding instruction)")
	issueIdx := strings.Index(got, "Originating issue (link only — fetch if you need detail):")
	if planIdx < 0 || issueIdx < 0 || planIdx > issueIdx {
		t.Errorf("plan should appear before issue link (planIdx=%d issueIdx=%d):\n%s",
			planIdx, issueIdx, got)
	}

	// The "implement the change described above" wording from the
	// pre-#223 prompt must be gone — the new wording leads with
	// the plan. A regression where both blocks rendered would be
	// confusing for the agent.
	if strings.Contains(got, "implement the change described above") {
		t.Errorf("legacy 'change described above' wording should be replaced when a plan is present:\n%s", got)
	}
}

func TestBuild_Implement_NoApprovedPlan_FallsBackToIssue(t *testing.T) {
	// Without a plan, behave exactly as the pre-#223 prompt did —
	// the historic baseline keeps non-issue-triggered runs working
	// and tolerates the race where the implement stage dispatches
	// before the plan artifact has propagated.
	got, err := Build("implement", Trigger{
		Source:      "github_issue",
		IssueNumber: 42,
		IssueTitle:  "Add foo",
		IssueBody:   "We need a foo helper.",
		Repo:        "kuhlman-labs/example",
		// ApprovedPlan deliberately nil.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if strings.Contains(got, "Approved plan") {
		t.Errorf("plan section leaked when ApprovedPlan was nil:\n%s", got)
	}
	if !strings.Contains(got, "Triggering issue: #42") {
		t.Errorf("issue context should still render as primary input:\n%s", got)
	}
	if !strings.Contains(got, "smallest set of changes") {
		t.Errorf("issue-only fallback wording missing:\n%s", got)
	}
}

func TestBuild_Implement_WithApprovedPlan_IsDeterministic(t *testing.T) {
	tr := Trigger{
		Source:       "github_issue",
		IssueNumber:  7,
		IssueTitle:   "T",
		IssueBody:    "B",
		Repo:         "o/r",
		ApprovedPlan: fixturePlan(),
	}
	a, _ := Build("implement", tr)
	b, _ := Build("implement", tr)
	if a != b {
		t.Error("Build with ApprovedPlan is non-deterministic across calls")
	}
}

func TestBuild_Plan_CitationOrTestRule(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		IssueTitle:  "Plan a refactor",
		Repo:        "x/y",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"citation",
		"test",
		"risks_and_assumptions",
		"SIGKILL",
		"cmd.Wait",
		"syscall.SysProcAttr",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan prompt missing citation-or-test rule string %q\n---\n%s", w, got)
		}
	}
}

func TestBuild_Plan_BudgetHintWithTimeouts(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber:           7,
		IssueTitle:            "Plan a refactor",
		Repo:                  "x/y",
		PlanStageTimeout:      30 * time.Minute,
		ImplementStageTimeout: 60 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"30 minutes",
		"60 minutes",
		"ADR-025",
		"decomposition.sub_plans",
		"predicted_runtime_minutes",
		"predicted_runtime_confidence",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan prompt missing %q\n---\n%s", w, got)
		}
	}
}

func TestBuild_Plan_BudgetHintDefaultFallback(t *testing.T) {
	// Zero durations should resolve to the default (15 minutes).
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		IssueTitle:  "Plan a refactor",
		Repo:        "x/y",
		// PlanStageTimeout and ImplementStageTimeout intentionally zero.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Both slots should show the default value.
	count := strings.Count(got, "15 minutes")
	if count < 2 {
		t.Errorf("expected 'plan stage 15 minutes, implement stage 15 minutes' in default prompt, got count=%d\n---\n%s", count, got)
	}
}

func TestBuild_Plan_NoCalibrationHint(t *testing.T) {
	got, err := Build("plan", Trigger{IssueNumber: 7, Repo: "x/y"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "### Calibration hint") {
		t.Errorf("plan prompt should not contain calibration hint when CalibrationHint is nil:\n%s", got)
	}
}

func TestBuild_Plan_CalibrationHintRendered(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		Repo:        "x/y",
		CalibrationHint: &CalibrationHint{
			Samples:          10,
			CalibrationRatio: 1.18,
			ActualP50Minutes: 12.5,
			ActualP95Minutes: 18.0,
			ConfidenceBands: map[string]CalibrationBand{
				"high":   {Samples: 4, WithinScale: 3},
				"medium": {Samples: 6, WithinScale: 4},
				"low":    {Samples: 2, WithinScale: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"### Calibration hint",
		"10 implement-stage",
		"actual p50 = 12.5 min",
		"p95 = 18.0 min",
		"ratio = 1.18",
		"high: 4 samples, 3 within 1.5x of prediction",
		"medium: 6 samples, 4 within 1.5x of prediction",
		"low: 2 samples, 2 within 1.5x of prediction",
		"Multiply your raw estimate by 1.18",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan prompt missing %q:\n%s", w, got)
		}
	}
	// Calibration hint must appear after the cmd.Wait counter-example.
	hintIdx := strings.Index(got, "### Calibration hint")
	waitIdx := strings.Index(got, "cmd.Wait")
	if hintIdx < 0 || waitIdx < 0 || hintIdx < waitIdx {
		t.Errorf("calibration hint should appear after cmd.Wait (hintIdx=%d waitIdx=%d):\n%s",
			hintIdx, waitIdx, got)
	}
}

func TestBuild_Plan_CalibrationHintRendered_RatioBelowOne(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		Repo:        "x/y",
		CalibrationHint: &CalibrationHint{
			Samples:          5,
			CalibrationRatio: 0.27,
			ConfidenceBands: map[string]CalibrationBand{
				"high": {Samples: 5, WithinScale: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Directional words must be absent — they mislead when ratio < 1.
	for _, bad := range []string{"overruns", "over ("} {
		if strings.Contains(got, bad) {
			t.Errorf("calibration hint should not contain directional word %q when ratio < 1:\n%s", bad, got)
		}
	}
	// Neutral multiplier phrase must be present.
	if !strings.Contains(got, "Multiply your raw estimate by 0.27") {
		t.Errorf("calibration hint missing neutral multiplier phrase:\n%s", got)
	}
}

func TestBuild_Implement_CalibrationHintIgnored(t *testing.T) {
	got, err := Build("implement", Trigger{
		Repo: "x/y",
		CalibrationHint: &CalibrationHint{
			Samples:          10,
			CalibrationRatio: 1.2,
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "### Calibration hint") {
		t.Errorf("implement prompt should not contain calibration hint:\n%s", got)
	}
}

func TestBuild_Plan_CalibrationHint_Deterministic(t *testing.T) {
	tr := Trigger{
		IssueNumber: 7,
		Repo:        "x/y",
		CalibrationHint: &CalibrationHint{
			Samples:          10,
			CalibrationRatio: 1.18,
			ConfidenceBands: map[string]CalibrationBand{
				"high":   {Samples: 4, WithinScale: 3},
				"medium": {Samples: 6, WithinScale: 4},
				"low":    {Samples: 2, WithinScale: 2},
			},
		},
	}
	a, _ := Build("plan", tr)
	b, _ := Build("plan", tr)
	if a != b {
		t.Errorf("Build with CalibrationHint is non-deterministic across calls:\nA: %s\nB: %s", a, b)
	}
}

func TestBuild_Plan_CalibrationHint_HighBandAdvisory(t *testing.T) {
	// High band at 1/10 within 1.5x (10% ≤ 25%) → advisory fires naming "high".
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		Repo:        "x/y",
		CalibrationHint: &CalibrationHint{
			Samples:          10,
			CalibrationRatio: 2.50,
			ActualP50Minutes: 25.0,
			ActualP95Minutes: 45.0,
			ConfidenceBands: map[string]CalibrationBand{
				"high": {Samples: 10, WithinScale: 1},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"\"high\" has been the LEAST accurate band historically",
		"1/10 within 1.5x",
		"Reserve \"high\" for genuinely mechanical changes",
		"Default to \"medium\"",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan prompt missing advisory string %q:\n%s", w, got)
		}
	}
}

func TestBuild_Plan_CalibrationHint_NoAdvisoryWhenHighAccurate(t *testing.T) {
	// Coverage: medium is the worst band (1/10) but high is accurate (8/10).
	// The advisory is gated on high specifically, so it must NOT fire here.
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		Repo:        "x/y",
		CalibrationHint: &CalibrationHint{
			Samples:          20,
			CalibrationRatio: 1.00,
			ActualP50Minutes: 10.0,
			ActualP95Minutes: 15.0,
			ConfidenceBands: map[string]CalibrationBand{
				"medium": {Samples: 10, WithinScale: 1},
				"high":   {Samples: 10, WithinScale: 8},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "LEAST accurate band historically") {
		t.Errorf("high-band advisory must not fire when high band is accurate (8/10):\n%s", got)
	}
}

func TestBuild_Plan_CalibrationHint_NoAdvisoryAboveThreshold(t *testing.T) {
	// All bands above 25% accuracy → no advisory.
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		Repo:        "x/y",
		CalibrationHint: &CalibrationHint{
			Samples:          30,
			CalibrationRatio: 1.00,
			ActualP50Minutes: 10.0,
			ActualP95Minutes: 15.0,
			ConfidenceBands: map[string]CalibrationBand{
				"high":   {Samples: 10, WithinScale: 4}, // 40% > 25%
				"medium": {Samples: 10, WithinScale: 4},
				"low":    {Samples: 10, WithinScale: 4},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "LEAST accurate band historically") {
		t.Errorf("advisory must not fire when all bands exceed 25%% accuracy:\n%s", got)
	}
}

func TestBuild_Plan_CalibrationHint_MediumBandAdvisory(t *testing.T) {
	// Medium band at 1/10 within 1.5x (10% ≤ 25%) → advisory fires naming
	// "medium" and surfacing the 1/ratio sizing-down factor. ratio 0.17 → ~5.9x.
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		Repo:        "x/y",
		CalibrationHint: &CalibrationHint{
			Samples:          10,
			CalibrationRatio: 0.17,
			ActualP50Minutes: 60.0,
			ActualP95Minutes: 90.0,
			ConfidenceBands: map[string]CalibrationBand{
				"medium": {Samples: 10, WithinScale: 1},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"\"medium\" has degraded too",
		"1/10 within 1.5x",
		"about 5.9x too high",
		"Drop to \"low\"",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan prompt missing medium advisory string %q:\n%s", w, got)
		}
	}
	// The medium advisory must never steer toward "high".
	if strings.Contains(got, "reaching for a higher band") == false {
		t.Errorf("medium advisory should steer away from higher bands:\n%s", got)
	}
}

func TestBuild_Plan_CalibrationHint_BothBandsBadFireBoth(t *testing.T) {
	// Both high and medium at 1/10 → both advisories fire independently.
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		Repo:        "x/y",
		CalibrationHint: &CalibrationHint{
			Samples:          20,
			CalibrationRatio: 0.50,
			ActualP50Minutes: 30.0,
			ActualP95Minutes: 50.0,
			ConfidenceBands: map[string]CalibrationBand{
				"high":   {Samples: 10, WithinScale: 1},
				"medium": {Samples: 10, WithinScale: 1},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"LEAST accurate band historically", // high-band advisory
		"\"medium\" has degraded too",      // medium-band advisory
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan prompt missing advisory string %q (both bands should fire):\n%s", w, got)
		}
	}
}

func TestBuild_Plan_CalibrationHint_NoMediumAdvisoryWhenAccurate(t *testing.T) {
	// Medium at 8/10 (80% > 25%) → medium advisory must NOT fire, while the
	// rest of the calibration hint still renders.
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		Repo:        "x/y",
		CalibrationHint: &CalibrationHint{
			Samples:          10,
			CalibrationRatio: 1.00,
			ActualP50Minutes: 10.0,
			ActualP95Minutes: 15.0,
			ConfidenceBands: map[string]CalibrationBand{
				"medium": {Samples: 10, WithinScale: 8},
			},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "\"medium\" has degraded too") {
		t.Errorf("medium-band advisory must not fire when medium band is accurate (8/10):\n%s", got)
	}
	// The hint body still renders.
	if !strings.Contains(got, "Confidence-band accuracy:") {
		t.Errorf("calibration hint body should still render:\n%s", got)
	}
}

func TestBuild_Plan_ScopeFilesShapeGuidance(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		IssueTitle:  "Plan a refactor",
		Repo:        "x/y",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"WRONG",
		"RIGHT",
		`"files": ["`,
		`"operation"`,
		"create",
		"modify",
		"delete",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan prompt missing scope.files shape guidance %q\n---\n%s", w, got)
		}
	}
}

func TestBuild_Plan_ContainsIncrementalVerification(t *testing.T) {
	got, err := Build("plan", Trigger{
		Source:      "github_issue",
		IssueNumber: 7,
		Repo:        "x/y",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "Incremental verification discipline") {
		t.Errorf("plan prompt missing 'Incremental verification discipline':\n%s", got)
	}
}

func TestBuild_Implement_BudgetContext_PlanPresent(t *testing.T) {
	got, err := Build("implement", Trigger{
		Repo:         "o/r",
		ApprovedPlan: fixturePlan(),
		PredictionContext: &PredictionContext{
			PredictedMinutes:    9,
			PredictedConfidence: "medium",
			StageBudgetMinutes:  30,
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, want := range []string{"### Budget context", "9 minutes", "medium confidence", "30 minutes"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, got)
		}
	}
}

func TestBuild_Implement_BudgetContext_NilContext(t *testing.T) {
	got, err := Build("implement", Trigger{
		Repo:         "o/r",
		ApprovedPlan: fixturePlan(),
		// PredictionContext deliberately nil.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "### Budget context") {
		t.Errorf("Budget context section should not appear when PredictionContext is nil:\n%s", got)
	}
}

func TestBuild_Implement_BudgetContext_DefaultBudget(t *testing.T) {
	got, err := Build("implement", Trigger{
		Repo:         "o/r",
		ApprovedPlan: fixturePlan(),
		PredictionContext: &PredictionContext{
			PredictedMinutes:    9,
			PredictedConfidence: "medium",
			StageBudgetMinutes:  0, // no spec budget → default 15m
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "### Budget context") {
		t.Errorf("Budget context section should appear even when StageBudgetMinutes is 0:\n%s", got)
	}
	if !strings.Contains(got, "15 minutes") {
		t.Errorf("prompt should contain default budget (15 minutes) when StageBudgetMinutes is 0:\n%s", got)
	}
}

func TestBuild_Plan_PriorRejectionFeedback_Rendered(t *testing.T) {
	feedback := "The plan lacked sufficient test coverage for edge cases."
	got, err := Build("plan", Trigger{
		IssueNumber:            7,
		Repo:                   "x/y",
		PriorRejectionFeedback: &feedback,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"### Prior plan-stage rejection feedback",
		"The operator rejected the most recent plan for this issue",
		"You MUST address this feedback in your new plan",
		feedback,
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan prompt missing %q:\n%s", w, got)
		}
	}
}

func TestBuild_Plan_PriorRejectionFeedback_Nil_SectionAbsent(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		Repo:        "x/y",
		// PriorRejectionFeedback deliberately nil.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "### Prior plan-stage rejection feedback") {
		t.Errorf("plan prompt should not contain rejection feedback section when nil:\n%s", got)
	}
}

func TestBuild_Plan_PriorRejectionFeedback_Truncated(t *testing.T) {
	// Input of 5000 bytes should be capped at 4000 bytes with the truncation suffix.
	// Cap is 4000 (not 2000) because real rejection rationales run 2-4KB —
	// substantive operator feedback shouldn't lose its actionable tail.
	longFeedback := strings.Repeat("x", 5000)
	got, err := Build("plan", Trigger{
		IssueNumber:            7,
		Repo:                   "x/y",
		PriorRejectionFeedback: &longFeedback,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "...[truncated]") {
		t.Errorf("plan prompt missing truncation suffix:\n%s", got)
	}
	// The full 5000-char string must not appear verbatim.
	if strings.Contains(got, longFeedback) {
		t.Errorf("untruncated long feedback appeared in prompt")
	}
}

func TestBuild_Plan_PriorSchemaValidationError_Rendered(t *testing.T) {
	validationErr := "scope.files[0]: expected object, got string"
	got, err := Build("plan", Trigger{
		IssueNumber:                7,
		Repo:                       "x/y",
		PriorSchemaValidationError: &validationErr,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"### Prior plan-stage schema validation failure",
		"Your previous plan failed standard_v1 validation",
		"Fix exactly this and re-emit a valid plan",
		validationErr,
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan prompt missing %q:\n%s", w, got)
		}
	}
}

func TestBuild_Plan_PriorSchemaValidationError_Nil_SectionAbsent(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		Repo:        "x/y",
		// PriorSchemaValidationError deliberately nil.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "### Prior plan-stage schema validation failure") {
		t.Errorf("plan prompt should not contain schema validation section when nil:\n%s", got)
	}
}

func TestBuild_Plan_PriorSchemaValidationError_Truncated(t *testing.T) {
	// Input over the 4000-byte cap must be truncated with the suffix,
	// mirroring PriorRejectionFeedback's maxFeedbackBytes pattern.
	longErr := strings.Repeat("x", 5000)
	got, err := Build("plan", Trigger{
		IssueNumber:                7,
		Repo:                       "x/y",
		PriorSchemaValidationError: &longErr,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "...[truncated]") {
		t.Errorf("plan prompt missing truncation suffix:\n%s", got)
	}
	if strings.Contains(got, longErr) {
		t.Errorf("untruncated long validation error appeared in prompt")
	}
}

func TestBuild_Implement_ScopeConstraint_Rendered(t *testing.T) {
	got, err := Build("implement", Trigger{
		Repo:         "o/r",
		ApprovedPlan: fixturePlan(),
		ScopeConstraint: &ScopeConstraint{
			ScopeHint:   "Implement the foo helper in pkg/bar.",
			ParentRunID: "00000000-0000-0000-0000-000000000001",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"SCOPE CONSTRAINT",
		"00000000-0000-0000-0000-000000000001",
		"Implement the foo helper in pkg/bar.",
		"Step zero",
		"list the files you intend to modify",
		"STOP and surface that the boundary is wrong",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("prompt missing %q\n---\n%s", w, got)
		}
	}
}

func TestBuild_Implement_ScopeConstraint_SiblingHints(t *testing.T) {
	got, err := Build("implement", Trigger{
		Repo:         "o/r",
		ApprovedPlan: fixturePlan(),
		ScopeConstraint: &ScopeConstraint{
			ScopeHint:    "Implement Part A in pkg/a.",
			ParentRunID:  "00000000-0000-0000-0000-000000000002",
			SiblingHints: []string{"Implement Part B in pkg/b.", "Implement Part C in pkg/c."},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, hint := range []string{"Implement Part B in pkg/b.", "Implement Part C in pkg/c."} {
		if !strings.Contains(got, hint) {
			t.Errorf("prompt missing sibling hint %q\n---\n%s", hint, got)
		}
	}
	if !strings.Contains(got, "do NOT modify code in sibling scope") {
		t.Errorf("prompt missing sibling prohibition notice\n---\n%s", got)
	}
}

func TestBuild_Implement_ScopeConstraint_Nil_SectionAbsent(t *testing.T) {
	got, err := Build("implement", Trigger{
		Repo:         "o/r",
		ApprovedPlan: fixturePlan(),
		// ScopeConstraint deliberately nil.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "SCOPE CONSTRAINT") {
		t.Errorf("SCOPE CONSTRAINT section should not appear when ScopeConstraint is nil:\n%s", got)
	}
}

func TestBuild_Implement_ScopeConstraint_AppearsBeforePlan(t *testing.T) {
	got, err := Build("implement", Trigger{
		Repo:         "o/r",
		ApprovedPlan: fixturePlan(),
		ScopeConstraint: &ScopeConstraint{
			ScopeHint:   "Implement the foo helper in pkg/bar.",
			ParentRunID: "00000000-0000-0000-0000-000000000003",
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	constraintIdx := strings.Index(got, "SCOPE CONSTRAINT")
	planIdx := strings.Index(got, "Approved plan (binding instruction)")
	if constraintIdx < 0 {
		t.Fatalf("SCOPE CONSTRAINT not found in prompt:\n%s", got)
	}
	if planIdx < 0 {
		t.Fatalf("Approved plan section not found in prompt:\n%s", got)
	}
	if constraintIdx > planIdx {
		t.Errorf("SCOPE CONSTRAINT should appear before the approved plan (constraintIdx=%d planIdx=%d):\n%s",
			constraintIdx, planIdx, got)
	}
}

func TestBuild_Plan_CompoundFieldDirective(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		IssueTitle:  "Plan a refactor",
		Repo:        "x/y",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The compound-field directive must explicitly name approach and
	// verification so agents don't produce bare-string values for
	// these structured fields.
	wants := []string{
		"Compound-field shape rule",
		"approach",
		"verification",
		"bare string",
		"decomposition.sub_plans[i]",
		"shorthand will be rejected",
		"do NOT set it to null",
		"the files THAT slice will touch",
		"narrows the fan-out child run's scope",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan prompt missing compound-field directive string %q\n---\n%s", w, got)
		}
	}
}

func TestBuild_Implement_ApprovalConditions_Rendered(t *testing.T) {
	cond := "add the cross-branch rejection test"
	got, err := Build("implement", Trigger{
		Repo:               "o/r",
		ApprovedPlan:       fixturePlan(),
		ApprovalConditions: &cond,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"### Approval conditions",
		"AMEND the plan",
		"MANDATORY",
		"win on conflict",
		cond,
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("prompt missing %q\n---\n%s", w, got)
		}
	}
	// Conditions must appear before the approved plan so the agent
	// sees them before reading the plan steps.
	condIdx := strings.Index(got, "### Approval conditions")
	planIdx := strings.Index(got, "Approved plan (binding instruction)")
	if condIdx < 0 || planIdx < 0 || condIdx > planIdx {
		t.Errorf("approval conditions should appear before approved plan (condIdx=%d planIdx=%d):\n%s",
			condIdx, planIdx, got)
	}
}

func TestBuild_Implement_ApprovalConditions_Nil_Absent(t *testing.T) {
	got, err := Build("implement", Trigger{
		Repo:         "o/r",
		ApprovedPlan: fixturePlan(),
		// ApprovalConditions deliberately nil.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "### Approval conditions") {
		t.Errorf("Approval conditions section should not appear when ApprovalConditions is nil:\n%s", got)
	}
}

func TestBuild_Implement_FixupConcerns_Rendered(t *testing.T) {
	concerns := []string{
		"[high/security] missing authz check on the fixup endpoint",
		"[medium/coverage] no test for the bound-exhausted path",
	}
	got, err := Build("implement", Trigger{
		Repo:          "o/r",
		ApprovedPlan:  fixturePlan(),
		FixupConcerns: concerns,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"### Fix-up concerns",
		"AMEND the plan",
		"MANDATORY",
		"win on conflict",
		concerns[0],
		concerns[1],
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("prompt missing %q\n---\n%s", w, got)
		}
	}
	// Concerns must appear before the approved plan so the agent sees the
	// binding instructions before reading the plan steps (mirrors #558).
	fixIdx := strings.Index(got, "### Fix-up concerns")
	planIdx := strings.Index(got, "Approved plan (binding instruction)")
	if fixIdx < 0 || planIdx < 0 || fixIdx > planIdx {
		t.Errorf("fix-up concerns should appear before approved plan (fixIdx=%d planIdx=%d):\n%s",
			fixIdx, planIdx, got)
	}
}

func TestBuild_Implement_FixupConcerns_Empty_Absent(t *testing.T) {
	got, err := Build("implement", Trigger{
		Repo:         "o/r",
		ApprovedPlan: fixturePlan(),
		// FixupConcerns deliberately nil.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "### Fix-up concerns") {
		t.Errorf("Fix-up concerns section should not appear when FixupConcerns is empty:\n%s", got)
	}
}

func TestBuild_Implement_FixupConcerns_Truncated(t *testing.T) {
	// One concern just under the cap, then more that must be dropped with a
	// truncation marker so a pathological concern set can't blow the prompt.
	concerns := []string{
		strings.Repeat("x", 3990),
		"this concern should be truncated",
		"so should this one",
	}
	got, err := Build("implement", Trigger{
		Repo:          "o/r",
		ApprovedPlan:  fixturePlan(),
		FixupConcerns: concerns,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "...[remaining concerns truncated]") {
		t.Errorf("expected truncation marker for oversized concern set:\n%s", got)
	}
	if strings.Contains(got, "so should this one") {
		t.Errorf("concerns past the byte cap should be dropped:\n%s", got)
	}
}

func TestBuild_PlanReview_ContainsVerdictSchema(t *testing.T) {
	got, err := Build("plan_review", Trigger{
		IssueNumber:  42,
		IssueTitle:   "Add foo",
		IssueBody:    "We need a foo function in pkg/bar.",
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Verdict schema must be present so the agent knows the output shape.
	wants := []string{
		`"verdict"`,
		`"approve"`,
		`"approve_with_concerns"`,
		`"reject"`,
		`"concerns"`,
		`"severity"`,
		`"category"`,
		`"note"`,
		`"free_form"`,
		"Verdict schema",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan_review prompt missing verdict schema element %q\n---\n%s", w, got)
		}
	}
}

func TestBuild_PlanReview_ContainsPlanArtifact(t *testing.T) {
	got, err := Build("plan_review", Trigger{
		IssueNumber:  42,
		IssueTitle:   "Add foo",
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The plan content must appear in the review prompt so the agent
	// can assess it.
	wants := []string{
		"Plan artifact",
		"Add a foo helper to pkg/bar.",
		"pkg/bar/foo.go (create)",
		"pkg/bar/bar.go (modify)",
		"1. Define Foo on the bar.Service interface.",
		"2. Implement Foo with a table-driven test.",
		"Test strategy:",
		"Rollback plan:",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan_review prompt missing plan content %q\n---\n%s", w, got)
		}
	}
}

func TestBuild_PlanReview_ContainsNoPlanConstraint(t *testing.T) {
	got, err := Build("plan_review", Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The no-re-plan constraint must be explicitly stated.
	noPlanStrings := []string{
		"ROLE CONSTRAINT",
		"Re-plan",
		"propose alternative plans",
		"suggest edits to the plan",
		"MUST NOT",
		"JSON only",
	}
	for _, w := range noPlanStrings {
		if !strings.Contains(got, w) {
			t.Errorf("plan_review prompt missing no-re-plan constraint %q\n---\n%s", w, got)
		}
	}
}

func TestBuild_PlanReview_ContainsIssueBody(t *testing.T) {
	got, err := Build("plan_review", Trigger{
		IssueNumber:  7,
		IssueTitle:   "Some issue",
		IssueBody:    "This is the issue body with context.",
		Repo:         "x/y",
		ApprovedPlan: fixturePlan(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Issue body must be present so the reviewer can assess whether
	// the plan actually addresses the originating issue.
	if !strings.Contains(got, "This is the issue body with context.") {
		t.Errorf("plan_review prompt should include the issue body for context:\n%s", got)
	}
	if !strings.Contains(got, "Originating issue") {
		t.Errorf("plan_review prompt missing 'Originating issue' section:\n%s", got)
	}
}

func TestBuild_PlanReview_NilPlan_EmitsMissingArtifactGuidance(t *testing.T) {
	got, err := Build("plan_review", Trigger{
		Repo: "x/y",
		// ApprovedPlan deliberately nil.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "no plan artifact provided") {
		t.Errorf("plan_review with nil plan should surface missing-artifact guidance:\n%s", got)
	}
	// Verdict schema must still be present even without a plan.
	if !strings.Contains(got, "Verdict schema") {
		t.Errorf("plan_review with nil plan must still include verdict schema:\n%s", got)
	}
}

func TestBuild_PlanReview_IsDeterministic(t *testing.T) {
	tr := Trigger{
		IssueNumber:  7,
		IssueTitle:   "T",
		IssueBody:    "B",
		Repo:         "o/r",
		ApprovedPlan: fixturePlan(),
	}
	a, _ := Build("plan_review", tr)
	b, _ := Build("plan_review", tr)
	if a != b {
		t.Errorf("Build plan_review is non-deterministic across calls:\nA: %s\nB: %s", a, b)
	}
}

func TestBuild_PlanReview_NoIssueContext_SectionAbsent(t *testing.T) {
	got, err := Build("plan_review", Trigger{
		Repo:         "x/y",
		ApprovedPlan: fixturePlan(),
		// No issue fields set.
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// When no issue context is available, the Originating issue section
	// should not appear — don't render an empty section header.
	if strings.Contains(got, "Originating issue") {
		t.Errorf("plan_review should not render Originating issue section when no issue context provided:\n%s", got)
	}
}

func TestBuild_PlanReview_ReviewCriteriaPresent(t *testing.T) {
	got, err := Build("plan_review", Trigger{
		Repo:         "x/y",
		ApprovedPlan: fixturePlan(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"Review criteria",
		"Scope completeness",
		"Approach feasibility",
		"Verification adequacy",
		"Risk coverage",
		"Schema compliance",
		"Cross-boundary integration test",
		"end-to-end",
		"Verdict decision rule",
		"approve_with_concerns",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan_review prompt missing review criteria element %q:\n%s", w, got)
		}
	}
}

// TestBuild_PlanReview_CrossBoundaryTestCriterion pins the load-bearing
// substrings of criterion #7 (#627). Mirrors
// TestBuild_PlanReview_GroundsRuleCitations. The criterion is advisory: it
// instructs the reviewer to record a concern (approve_with_concerns) when a
// cross-boundary change lacks an end-to-end test, not to hard-reject.
func TestBuild_PlanReview_CrossBoundaryTestCriterion(t *testing.T) {
	got, err := Build("plan_review", Trigger{
		Repo:         "x/y",
		ApprovedPlan: fixturePlan(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"serialization boundary",
		"absent from scope.files",
		"unit-only",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan_review prompt missing cross-boundary criterion substring %q:\n%s", w, got)
		}
	}
}

func TestBuild_PlanReview_TrimmedBelowBaseline(t *testing.T) {
	// #606: the verbose verdict-schema / review-criteria / decision-rule
	// preamble was trimmed to lower the per-call token cost on the local
	// reviewer (no prompt caching — the full prompt is one -p argument). Pin
	// the size reduction so a future reword can't silently re-bloat the
	// prompt. The baseline below is the byte length of the prompt (with this
	// fixture) BEFORE the #606 trim. Load-bearing tokens are covered by the
	// other TestBuild_PlanReview_* tests; this one guards the token budget.
	//
	// #627 raised this baseline to keep the trim-guard meaningful after
	// criterion #7 (cross-boundary integration test) was added. The guard's
	// meaning is "the #606 trim still removes >= minReduction bytes vs the
	// untrimmed version" — so the baseline must represent the untrimmed prompt
	// that ALSO includes criterion #7, NOT current_size + minReduction (which
	// would make the assertion tautological). New baseline = the original
	// pre-#606 untrimmed length (3333) PLUS the 583 bytes criterion #7 adds.
	const preTrimBaselineLen = 3916
	got := buildPlanReview(Trigger{
		Repo:         "kuhlman-labs/example",
		IssueNumber:  42,
		IssueTitle:   "Add foo",
		IssueBody:    "We need a foo function in pkg/bar.",
		ApprovedPlan: fixturePlan(),
	})
	if len(got) >= preTrimBaselineLen {
		t.Errorf("buildPlanReview not trimmed: got %d bytes, expected materially below pre-trim baseline %d",
			len(got), preTrimBaselineLen)
	}
	// Require a material reduction, not a one-byte cosmetic change.
	const minReduction = 300
	if preTrimBaselineLen-len(got) < minReduction {
		t.Errorf("buildPlanReview trim immaterial: got %d bytes, only %d below baseline %d (want >= %d shorter)",
			len(got), preTrimBaselineLen-len(got), preTrimBaselineLen, minReduction)
	}
}

func TestBuild_PlanReview_GroundsRuleCitations(t *testing.T) {
	// #595: review agents fabricated a CLAUDE.md comment-length rule that
	// does not exist. The grounding constraint must instruct the reviewer to
	// only cite rules it can quote verbatim from provided context, never from
	// memory. Pin the load-bearing substrings.
	got, err := Build("plan_review", Trigger{
		Repo:         "x/y",
		ApprovedPlan: fixturePlan(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"Grounded citations",
		"quote verbatim",
		"CLAUDE.md",
		"Do NOT assert rules from memory",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan_review prompt missing grounding-constraint substring %q:\n%s", w, got)
		}
	}
}

func TestBuild_PlanReview_ProducesNoPRDescriptionGuidance(t *testing.T) {
	got, err := Build("plan_review", Trigger{
		Repo:         "x/y",
		ApprovedPlan: fixturePlan(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The review prompt must not bleed in implement-stage instructions.
	// The agent is a reviewer, not an implementer.
	if strings.Contains(got, PullRequestDescriptionPath) {
		t.Errorf("plan_review prompt must not include PR description guidance:\n%s", got)
	}
	if strings.Contains(got, "## Summary") {
		t.Errorf("plan_review prompt must not include PR section headers:\n%s", got)
	}
}

func TestBuild_PlanReview_UnsupportedStageStillErrors(t *testing.T) {
	// Confirm that adding plan_review didn't accidentally break the
	// ErrUnsupportedStage path for truly unknown stage types.
	_, err := Build("deploy", Trigger{})
	if !errors.Is(err, ErrUnsupportedStage) {
		t.Errorf("expected ErrUnsupportedStage for 'deploy', got %v", err)
	}
}

func TestBuild_Implement_WithSparsePlan_OmitsEmptySections(t *testing.T) {
	// A plan that fails optional sections (no scope.files, no
	// risks) should still render cleanly — empty sections drop
	// rather than printing dangling headers.
	sparse := &plan.Plan{
		PlanVersion: "standard_v1",
		Summary:     "tiny change",
		Verification: plan.Verification{
			TestStrategy: "ts",
			RollbackPlan: "rb",
		},
	}
	got, err := Build("implement", Trigger{
		Repo:         "o/r",
		ApprovedPlan: sparse,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "Files in scope:") {
		t.Errorf("Files header should drop on empty Scope.Files:\n%s", got)
	}
	if strings.Contains(got, "Approach:") {
		t.Errorf("Approach header should drop on empty Approach:\n%s", got)
	}
	if strings.Contains(got, "Risks & assumptions:") {
		t.Errorf("Risks header should drop on empty RisksAndAssumptions:\n%s", got)
	}
	if !strings.Contains(got, "tiny change") {
		t.Errorf("summary should still render:\n%s", got)
	}
}

func TestBuildPlanReview_ContainsSplitMarker(t *testing.T) {
	got := buildPlanReview(Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
	})
	if !strings.Contains(got, PlanReviewSplitMarker) {
		t.Errorf("buildPlanReview output missing PlanReviewSplitMarker %q", PlanReviewSplitMarker)
	}
}

func TestBuild_ImplementReview_FullContext(t *testing.T) {
	got, err := Build("implement_review", Trigger{
		Repo:         "kuhlman-labs/example",
		IssueNumber:  42,
		IssueTitle:   "Add foo",
		IssueBody:    "We need a foo function in pkg/bar.",
		ApprovedPlan: fixturePlan(),
		Diff:         "- M pkg/bar/bar.go\n- A pkg/bar/foo.go\n",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		// Role constraint + JSON-only contract.
		"ROLE CONSTRAINT",
		"single JSON object",
		// The diff under review renders the changed files.
		ImplementReviewSplitMarker,
		"pkg/bar/foo.go",
		// Honest framing: it's a changed-files list, not a line-level diff,
		// and the reviewer must read files for content (#585).
		"NOT a line-level diff",
		"READ each listed file",
		// scope.files from the approved plan (for drift comparison).
		"pkg/bar/legacy.go (delete)",
		// Verdict schema closed set.
		"\"approve\" | \"approve_with_concerns\" | \"reject\"",
		// scope-drift flag-only instruction.
		"Scope adherence (flag-only)",
		"Do NOT reject solely for scope drift",
		"Scope drift ALONE is never grounds for reject",
		// Issue context.
		"Issue: #42 · Add foo",
		"We need a foo function in pkg/bar.",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("implement_review prompt missing %q:\n%s", w, got)
		}
	}
}

func TestBuild_ImplementReview_GroundsRuleCitationsAndScopesStyle(t *testing.T) {
	// #595: on run 112743b1 the implement-review raised {category:scope}
	// concerns asserting a CLAUDE.md comment-length rule that does not exist
	// and flagged compliant multi-line WHY comments. The grounding constraint
	// and the style-is-lint scoping line must both be present.
	got, err := Build("implement_review", Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
		Diff:         "- M pkg/bar/bar.go\n",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		// Grounding constraint.
		"Grounded citations",
		"quote verbatim",
		"CLAUDE.md",
		"Do NOT assert rules from memory",
		// Style-is-lint scoping.
		"Style is out of scope",
		"comment length, naming aesthetics, formatting",
		"that is lint's job",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("implement_review prompt missing substring %q:\n%s", w, got)
		}
	}
}

func TestBuild_ImplementReview_OrthogonalLenses(t *testing.T) {
	// #703: the implement-review prompt is re-aimed at the lenses the
	// deterministic gates (policy gate, test suite, build/lint, CI) cannot
	// see — security/authz, test vacuity, and untested error/edge/concurrency
	// paths — and explicitly stops re-verifying plan adherence or
	// generic-bug-hunting. The security lens is self-gating on low-risk diffs.
	got, err := Build("implement_review", Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
		Diff:         "- M pkg/bar/bar.go\n",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		// Explicit non-goals.
		"Do NOT re-verify plan adherence",
		"Do NOT generic-bug-hunt",
		// The three orthogonal lenses.
		"Security / authz",
		"lethal trifecta",
		"Test vacuity",
		"vacuous",
		"Untested error / edge / concurrency paths",
		// Self-gating escape on a low-risk diff.
		"if the diff touches NO sensitive surface",
		"manufacture a security concern for a low-risk diff",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("implement_review prompt missing %q:\n%s", w, got)
		}
	}
	// Decision rule is re-anchored on the new lenses, not plan adherence.
	if !strings.Contains(got, "a security / authz regression, a vacuous test") {
		t.Errorf("verdict decision rule not re-aimed at the new lenses:\n%s", got)
	}
	// Determinism still holds across replays.
	again, _ := Build("implement_review", Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
		Diff:         "- M pkg/bar/bar.go\n",
	})
	if got != again {
		t.Errorf("implement_review prompt is non-deterministic across calls")
	}
}

func TestBuild_ImplementReview_ScopeDrift_RendersSection(t *testing.T) {
	// #695: when the trace handler threads runner-reported scope_drift paths
	// onto the Trigger, the implement-review prompt names them flagged
	// "operator may stage" so the reviewer does not false-reject a required
	// file that landed via a drifted path.
	got, err := Build("implement_review", Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
		Diff:         "- M pkg/bar/bar.go\n",
		ScopeDrift:   []string{"pkg/bar/bar_test.go", "docs/notes.md"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, w := range []string{
		"Scope drift (excluded from the diff above — operator may stage)",
		"pkg/bar/bar_test.go",
		"docs/notes.md",
		"Do NOT treat any of these paths as missing",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("scope-drift prompt missing %q:\n%s", w, got)
		}
	}
}

func TestBuild_ImplementReview_StandingAntiFalseRejectRule_AlwaysPresent(t *testing.T) {
	// #695: the standing anti-false-reject rule applies whether or not a
	// drift list is present, so it must render even with ScopeDrift empty —
	// the path list is an enhancement, the rule is the correctness backstop.
	got, err := Build("implement_review", Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
		Diff:         "- M pkg/bar/bar.go\n",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The drift section itself is guarded by len>0, so it must be absent.
	if strings.Contains(got, "Scope drift (excluded from the diff above") {
		t.Errorf("drift section should be absent when ScopeDrift is empty:\n%s", got)
	}
	for _, w := range []string{
		"Do NOT reject on an unconfirmable absence (standing rule)",
		"Treat an absence you cannot positively confirm as unverifiable",
		"do not assert the absence of a file you could not actually inspect",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("standing anti-false-reject rule missing %q:\n%s", w, got)
		}
	}
}

func TestBuild_ImplementReview_WithPatch_RendersHunks(t *testing.T) {
	patch := "diff --git a/pkg/bar/bar.go b/pkg/bar/bar.go\n" +
		"@@ -1,3 +1,3 @@\n-old line\n+new line\n"
	got, err := Build("implement_review", Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
		Diff:         "- M pkg/bar/bar.go\n",
		DiffPatch:    patch,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The split marker still leads the section.
	if !strings.Contains(got, ImplementReviewSplitMarker) {
		t.Errorf("split marker missing:\n%s", got)
	}
	// Real hunks are rendered, and the file list survives as an index.
	for _, w := range []string{
		"-old line",
		"+new line",
		"@@ -1,3 +1,3 @@",
		"index for the hunks below",
		"both added and removed lines are visible above",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("patch-present prompt missing %q:\n%s", w, got)
		}
	}
	// The original #561 file-list caveat is REVISED out on the patch
	// path — the reviewer can inspect lines directly, so we must not
	// tell them deleted lines are invisible.
	if strings.Contains(got, "do not assert the absence of regressions you could not actually inspect") {
		t.Errorf("patch-present prompt must not keep the file-list-only caveat:\n%s", got)
	}
}

func TestBuild_ImplementReview_WithoutPatch_KeepsOriginalCaveatVerbatim(t *testing.T) {
	// Backward-compat: no DiffPatch (older bundle / patch-compute
	// failure / size cap) falls back to the file-list rendering with the
	// original #561 caveat verbatim.
	got, err := Build("implement_review", Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
		Diff:         "- M pkg/bar/bar.go\n",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, w := range []string{
		"NOT a line-level diff",
		"READ each listed file",
		"do not assert the absence of regressions you could not actually inspect",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("fallback prompt missing original caveat %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "```diff") {
		t.Errorf("fallback prompt should not render a diff fence:\n%s", got)
	}
}

func TestBuild_ImplementReview_EmptyDiff_NotesEmptyDiff(t *testing.T) {
	got, err := Build("implement_review", Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "no diff present") {
		t.Errorf("implement_review prompt should note an empty diff:\n%s", got)
	}
}

func TestBuild_ImplementReview_ProducesNoPRDescriptionGuidance(t *testing.T) {
	got, err := Build("implement_review", Trigger{
		Repo:         "kuhlman-labs/example",
		ApprovedPlan: fixturePlan(),
		Diff:         "- M pkg/bar/bar.go\n",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, PullRequestDescriptionPath) {
		t.Errorf("implement_review prompt must not carry implement-stage PR guidance:\n%s", got)
	}
}

// TestBuild_PlanReview_IssueCommentsRendered is the #622 acceptance check
// for the plan-review path: the reviewer must see the same comment-borne
// refinements the planner saw, with the supersede preface, author +
// timestamp prefixes, and chronological order after the body.
func TestBuild_PlanReview_IssueCommentsRendered(t *testing.T) {
	got, err := Build("plan_review", Trigger{
		IssueNumber:  616,
		IssueTitle:   "Add a foo flag",
		IssueBody:    "We need a --foo flag that defaults to off.",
		Repo:         "x/y",
		ApprovedPlan: fixturePlan(),
		IssueComments: []IssueComment{
			{Author: "alice", Body: "First thought: make it a bool.", CreatedAt: "2026-05-01T10:00:00Z"},
			{Author: "bob", Body: "Correction: --foo must default to ON, not off.", CreatedAt: "2026-05-02T12:30:00Z"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"### Issue comments",
		"supersede", // the preface
		"**@alice** (2026-05-01T10:00:00Z):",
		"First thought: make it a bool.",
		"**@bob** (2026-05-02T12:30:00Z):",
		"Correction: --foo must default to ON, not off.",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan_review prompt missing %q\n---\n%s", w, got)
		}
	}
	// Chronological order: body, then alice, then bob.
	bodyIdx := strings.Index(got, "We need a --foo flag")
	aliceIdx := strings.Index(got, "**@alice**")
	bobIdx := strings.Index(got, "**@bob**")
	if bodyIdx >= aliceIdx || aliceIdx >= bobIdx {
		t.Errorf("expected body < alice < bob ordering, got body=%d alice=%d bob=%d", bodyIdx, aliceIdx, bobIdx)
	}
}

// TestBuild_PlanReview_AllBotComments_SectionAbsent confirms the
// plan-review prompt is byte-identical to the no-comments case when every
// comment is bot-authored — the body-only review prompt is unchanged.
func TestBuild_PlanReview_AllBotComments_SectionAbsent(t *testing.T) {
	got, err := Build("plan_review", Trigger{
		IssueNumber:  7,
		IssueTitle:   "T",
		IssueBody:    "Body stays.",
		Repo:         "x/y",
		ApprovedPlan: fixturePlan(),
		IssueComments: []IssueComment{
			{Author: "github-actions[bot]", Body: "CI failed.", CreatedAt: "2026-05-01T00:00:00Z"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "### Issue comments") {
		t.Errorf("plan_review section must be absent when all comments are bot-authored:\n%s", got)
	}
	if !strings.Contains(got, "Body stays.") {
		t.Errorf("plan_review body-only fallback should be unchanged:\n%s", got)
	}
}

// TestBuild_ImplementReview_IssueCommentsRendered is the #622 acceptance
// check for the implement-review path: the reviewer sees the comment-borne
// refinements with preface, author + timestamp, and chronological order.
func TestBuild_ImplementReview_IssueCommentsRendered(t *testing.T) {
	got, err := Build("implement_review", Trigger{
		IssueNumber:  616,
		IssueTitle:   "Add a foo flag",
		IssueBody:    "We need a --foo flag that defaults to off.",
		Repo:         "x/y",
		ApprovedPlan: fixturePlan(),
		Diff:         "- M pkg/foo/foo.go\n",
		IssueComments: []IssueComment{
			{Author: "alice", Body: "First thought: make it a bool.", CreatedAt: "2026-05-01T10:00:00Z"},
			{Author: "bob", Body: "Correction: --foo must default to ON, not off.", CreatedAt: "2026-05-02T12:30:00Z"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"### Issue comments",
		"supersede",
		"**@alice** (2026-05-01T10:00:00Z):",
		"First thought: make it a bool.",
		"**@bob** (2026-05-02T12:30:00Z):",
		"Correction: --foo must default to ON, not off.",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("implement_review prompt missing %q\n---\n%s", w, got)
		}
	}
	bodyIdx := strings.Index(got, "We need a --foo flag")
	aliceIdx := strings.Index(got, "**@alice**")
	bobIdx := strings.Index(got, "**@bob**")
	if bodyIdx >= aliceIdx || aliceIdx >= bobIdx {
		t.Errorf("expected body < alice < bob ordering, got body=%d alice=%d bob=%d", bodyIdx, aliceIdx, bobIdx)
	}
}

// TestBuild_ImplementReview_NilComments_SectionAbsent confirms the
// implement-review body-only fallback is unchanged when no comments are
// present (the pre-#622 shape).
func TestBuild_ImplementReview_NilComments_SectionAbsent(t *testing.T) {
	got, err := Build("implement_review", Trigger{
		IssueNumber:  7,
		IssueTitle:   "T",
		IssueBody:    "Just the body.",
		Repo:         "x/y",
		ApprovedPlan: fixturePlan(),
		Diff:         "- M pkg/foo/foo.go\n",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "### Issue comments") {
		t.Errorf("implement_review section expected absent for nil IssueComments:\n%s", got)
	}
	if !strings.Contains(got, "Just the body.") {
		t.Errorf("implement_review body should still render:\n%s", got)
	}
}

// TestBuild_Plan_IssueCommentsRendered is the headline #618 / #616
// acceptance check: a comment that contradicts the body renders in the
// '### Issue comments' section with its author + timestamp and the
// supersede preface, chronologically after the body.
func TestBuild_Plan_IssueCommentsRendered(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 616,
		IssueTitle:  "Add a foo flag",
		IssueBody:   "We need a --foo flag that defaults to off.",
		Repo:        "x/y",
		IssueComments: []IssueComment{
			{Author: "alice", Body: "First thought: make it a bool.", CreatedAt: "2026-05-01T10:00:00Z"},
			{Author: "bob", Body: "Correction: --foo must default to ON, not off.", CreatedAt: "2026-05-02T12:30:00Z"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"### Issue comments",
		"supersede", // the preface
		"**@alice** (2026-05-01T10:00:00Z):",
		"First thought: make it a bool.",
		"**@bob** (2026-05-02T12:30:00Z):",
		"Correction: --foo must default to ON, not off.",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("plan prompt missing %q\n---\n%s", w, got)
		}
	}
	// Chronological order: body, then alice, then bob.
	bodyIdx := strings.Index(got, "We need a --foo flag")
	aliceIdx := strings.Index(got, "**@alice**")
	bobIdx := strings.Index(got, "**@bob**")
	if bodyIdx >= aliceIdx || aliceIdx >= bobIdx {
		t.Errorf("expected body < alice < bob ordering, got body=%d alice=%d bob=%d", bodyIdx, aliceIdx, bobIdx)
	}
}

// TestBuild_Plan_BotCommentsFiltered confirms comments authored by a
// login ending in [bot] (CI bots, Fishhawk's own #377 footer) are
// dropped from the rendered section while human comments survive.
func TestBuild_Plan_BotCommentsFiltered(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		IssueTitle:  "T",
		IssueBody:   "Body.",
		Repo:        "x/y",
		IssueComments: []IssueComment{
			{Author: "github-actions[bot]", Body: "CI failed on main.", CreatedAt: "2026-05-01T00:00:00Z"},
			{Author: "carol", Body: "Human refinement here.", CreatedAt: "2026-05-02T00:00:00Z"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "CI failed on main.") || strings.Contains(got, "github-actions[bot]") {
		t.Errorf("bot-authored comment should be filtered:\n%s", got)
	}
	if !strings.Contains(got, "Human refinement here.") {
		t.Errorf("human comment should survive the bot filter:\n%s", got)
	}
}

// TestBuild_Plan_AllBotComments_SectionAbsent guards the distinct case
// where EVERY comment is bot-authored: the '### Issue comments' section
// is absent entirely (not rendered empty) and the body-only fallback is
// unchanged. Distinct from the nil-slice case below.
func TestBuild_Plan_AllBotComments_SectionAbsent(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		IssueTitle:  "T",
		IssueBody:   "Body stays.",
		Repo:        "x/y",
		IssueComments: []IssueComment{
			{Author: "github-actions[bot]", Body: "CI failed.", CreatedAt: "2026-05-01T00:00:00Z"},
			{Author: "dependabot[bot]", Body: "Bump dep.", CreatedAt: "2026-05-02T00:00:00Z"},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "### Issue comments") {
		t.Errorf("section must be absent when all comments are bot-authored:\n%s", got)
	}
	if !strings.Contains(got, "Body stays.") {
		t.Errorf("body-only fallback should be unchanged:\n%s", got)
	}
}

// TestBuild_Plan_NoIssueComments confirms the body-only fallback is
// unchanged when IssueComments is nil (the pre-#618 shape).
func TestBuild_Plan_NoIssueComments(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		IssueTitle:  "T",
		IssueBody:   "Just the body.",
		Repo:        "x/y",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(got, "### Issue comments") {
		t.Errorf("no comments section expected for nil IssueComments:\n%s", got)
	}
	if !strings.Contains(got, "Just the body.") {
		t.Errorf("body should still render:\n%s", got)
	}
}

// TestBuild_Plan_PerCommentTruncation confirms an over-cap comment body
// is truncated with the ...[truncated] marker.
func TestBuild_Plan_PerCommentTruncation(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	got, err := Build("plan", Trigger{
		IssueNumber:   7,
		IssueBody:     "Body.",
		Repo:          "x/y",
		IssueComments: []IssueComment{{Author: "alice", Body: huge, CreatedAt: "2026-05-01T00:00:00Z"}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "...[truncated]") {
		t.Errorf("expected per-comment truncation marker:\n%s", got)
	}
	if strings.Contains(got, huge) {
		t.Error("full over-cap comment body should not appear verbatim")
	}
}

// TestBuild_Plan_TotalBudgetDropsOldest confirms that when the total
// comment budget is exceeded, the OLDEST comments are dropped first
// (recency is load-bearing) and the omission marker is prepended. The
// newest comment always survives.
func TestBuild_Plan_TotalBudgetDropsOldest(t *testing.T) {
	// Each comment is ~1900 bytes (under the 2000 per-comment cap); 10
	// of them (~19KB) blows past the 12KB total budget so the oldest
	// get dropped.
	var comments []IssueComment
	for i := 0; i < 10; i++ {
		comments = append(comments, IssueComment{
			Author:    "u",
			Body:      strings.Repeat("a", 1900) + "_comment" + string(rune('0'+i)),
			CreatedAt: "2026-05-01T00:00:0" + string(rune('0'+i)) + "Z",
		})
	}
	got, err := Build("plan", Trigger{
		IssueNumber:   7,
		IssueBody:     "Body.",
		Repo:          "x/y",
		IssueComments: comments,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "older comment(s) omitted to fit budget") {
		t.Errorf("expected omission marker when over total budget:\n%s", got)
	}
	// Newest (index 9) survives; oldest (index 0) is dropped.
	if !strings.Contains(got, "_comment9") {
		t.Errorf("newest comment must survive:\n%s", got)
	}
	if strings.Contains(got, "_comment0") {
		t.Errorf("oldest comment should be dropped when over budget:\n%s", got)
	}
}
