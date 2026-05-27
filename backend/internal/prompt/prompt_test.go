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
		"actual runtime was 1.18x of predicted",
		"10 implement-stage",
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
