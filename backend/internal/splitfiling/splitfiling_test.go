package splitfiling

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// threePhaseProposal is the canonical expand→migrate→contract fixture.
func threePhaseProposal() plan.SplitProposal {
	return plan.SplitProposal{
		Rationale: "scope.files exceeds the implement cap by count",
		Phases: []plan.SplitPhase{
			{
				Title:     "expand: add new names alongside old",
				ScopeHint: "add NewFoo alongside Foo in backend/internal/foo",
				Scope:     &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify}}},
			},
			{
				Title:     "migrate: move consumers to new names",
				ScopeHint: "migrate consumers of Foo to NewFoo across backend/internal",
				Scope:     &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/bar/bar.go", Operation: plan.FileOpModify}}},
				DependsOn: []int{0},
			},
			{
				Title:     "contract: delete the transitional names",
				ScopeHint: "delete Foo now that all consumers use NewFoo",
				Scope:     &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpDelete}}},
				DependsOn: []int{1},
			},
		},
	}
}

func TestBuildChildSpecs_NPhasesNSpecsContractFlagOnLast(t *testing.T) {
	specs := BuildChildSpecs(BuildInput{
		Proposal:         threePhaseProposal(),
		ParentIssue:      2100,
		ParentAcceptance: []string{"the old name Foo no longer exists", "all callers use NewFoo"},
	})
	if got, want := len(specs), 3; got != want {
		t.Fatalf("len(specs) = %d, want %d", got, want)
	}
	for i, s := range specs {
		if s.PhaseIndex != i {
			t.Errorf("specs[%d].PhaseIndex = %d, want %d", i, s.PhaseIndex, i)
		}
		wantContract := i == len(specs)-1
		if s.IsContract != wantContract {
			t.Errorf("specs[%d].IsContract = %v, want %v", i, s.IsContract, wantContract)
		}
	}
}

func TestBuildChildSpecs_SymbolSetScopeFromHintNotFileList(t *testing.T) {
	specs := BuildChildSpecs(BuildInput{Proposal: threePhaseProposal(), ParentIssue: 2100})
	migrate := specs[1]
	if !strings.Contains(migrate.ScopeStatement, "migrate consumers of Foo to NewFoo") {
		t.Errorf("scope statement %q should carry the scope_hint prose", migrate.ScopeStatement)
	}
	// A stale file list must NOT leak into the symbol-set scope statement.
	if strings.Contains(migrate.ScopeStatement, "bar.go") {
		t.Errorf("scope statement %q must not embed a raw file list", migrate.ScopeStatement)
	}
}

func TestBuildChildSpecs_ScopeStatementFallsBackToPackagesWhenHintAbsent(t *testing.T) {
	proposal := plan.SplitProposal{Phases: []plan.SplitPhase{{
		Title: "only phase",
		Scope: &plan.Scope{Files: []plan.ScopeFile{
			{Path: "backend/internal/foo/foo.go", Operation: plan.FileOpModify},
			{Path: "backend/internal/foo/bar.go", Operation: plan.FileOpModify},
		}},
	}}}
	specs := BuildChildSpecs(BuildInput{Proposal: proposal, ParentIssue: 1})
	stmt := specs[0].ScopeStatement
	if !strings.Contains(stmt, "backend/internal/foo") {
		t.Errorf("fallback scope statement %q should name the affected package", stmt)
	}
	if strings.Contains(stmt, "foo.go") || strings.Contains(stmt, "bar.go") {
		t.Errorf("fallback scope statement %q must be package-level prose, not a file list", stmt)
	}
}

func TestBuildChildSpecs_DependsOnPreserved(t *testing.T) {
	specs := BuildChildSpecs(BuildInput{Proposal: threePhaseProposal(), ParentIssue: 1})
	if len(specs[0].DependsOn) != 0 {
		t.Errorf("phase 0 depends_on = %v, want empty", specs[0].DependsOn)
	}
	if got := specs[1].DependsOn; len(got) != 1 || got[0] != 0 {
		t.Errorf("phase 1 depends_on = %v, want [0]", got)
	}
	if got := specs[2].DependsOn; len(got) != 1 || got[0] != 1 {
		t.Errorf("phase 2 depends_on = %v, want [1]", got)
	}
}

func TestBuildChildSpecs_ParentAndDesignRefsPresent(t *testing.T) {
	specs := BuildChildSpecs(BuildInput{Proposal: threePhaseProposal(), ParentIssue: 2100})
	for i, s := range specs {
		if s.ParentIssue != 2100 {
			t.Errorf("specs[%d].ParentIssue = %d, want 2100", i, s.ParentIssue)
		}
		if s.DesignIssue != DesignIssue {
			t.Errorf("specs[%d].DesignIssue = %d, want %d", i, s.DesignIssue, DesignIssue)
		}
		if !strings.Contains(s.Proposal, "#2100") {
			t.Errorf("specs[%d].Proposal should reference parent #2100: %q", i, s.Proposal)
		}
		if !strings.Contains(s.Proposal, "#2008") {
			t.Errorf("specs[%d].Proposal should reference design #2008: %q", i, s.Proposal)
		}
	}
}

func TestBuildChildSpecs_DesignRefOverride(t *testing.T) {
	specs := BuildChildSpecs(BuildInput{Proposal: threePhaseProposal(), ParentIssue: 1, DesignIssue: 4242})
	if specs[0].DesignIssue != 4242 {
		t.Errorf("DesignIssue override = %d, want 4242", specs[0].DesignIssue)
	}
	if !strings.Contains(specs[0].Proposal, "#4242") {
		t.Errorf("proposal should reference overridden design #4242: %q", specs[0].Proposal)
	}
}

func TestBuildChildSpecs_ContractChildCarriesAcceptanceAndNoCloses(t *testing.T) {
	ac := []string{"the old name Foo no longer exists", "all callers use NewFoo"}
	specs := BuildChildSpecs(BuildInput{
		Proposal:         threePhaseProposal(),
		ParentIssue:      2100,
		ParentAcceptance: ac,
	})
	contract := specs[len(specs)-1]
	if !contract.IsContract {
		t.Fatal("last spec should be the contract child")
	}
	if len(contract.AcceptanceCriteria) != len(ac) {
		t.Fatalf("contract child AcceptanceCriteria = %v, want %v", contract.AcceptanceCriteria, ac)
	}
	for i := range ac {
		if contract.AcceptanceCriteria[i] != ac[i] {
			t.Errorf("AcceptanceCriteria[%d] = %q, want %q", i, contract.AcceptanceCriteria[i], ac[i])
		}
	}
	// The contract child must reference the parent but carry NO "Closes #" token.
	body := contract.Proposal + "\n" + contract.DoneMeans + "\n" + strings.Join(contract.AcceptanceCriteria, "\n")
	if strings.Contains(body, "Closes #") {
		t.Errorf("contract child body must not contain a 'Closes #' line: %q", body)
	}
	if !strings.Contains(contract.Proposal, "#2062") {
		t.Errorf("contract child should reference the #2062 deferral: %q", contract.Proposal)
	}
}

func TestBuildChildSpecs_NonContractChildrenReferenceButDoNotCloseParent(t *testing.T) {
	specs := BuildChildSpecs(BuildInput{Proposal: threePhaseProposal(), ParentIssue: 2100})
	for i, s := range specs {
		if s.IsContract {
			continue
		}
		if len(s.AcceptanceCriteria) != 0 {
			t.Errorf("non-contract specs[%d] should not carry acceptance criteria, got %v", i, s.AcceptanceCriteria)
		}
		if !strings.Contains(s.Proposal, "#2100") {
			t.Errorf("non-contract specs[%d] should reference parent #2100: %q", i, s.Proposal)
		}
		if strings.Contains(s.Proposal, "Closes #") {
			t.Errorf("non-contract specs[%d] body must not close the parent: %q", i, s.Proposal)
		}
	}
}

func TestBuildChildSpecs_EmptyProposalYieldsNoSpecs(t *testing.T) {
	specs := BuildChildSpecs(BuildInput{Proposal: plan.SplitProposal{}, ParentIssue: 1})
	if len(specs) != 0 {
		t.Errorf("empty proposal should yield no specs, got %d", len(specs))
	}
}

func TestBuildChildSpecs_SinglePhaseIsContract(t *testing.T) {
	proposal := plan.SplitProposal{Phases: []plan.SplitPhase{{Title: "only", ScopeHint: "x"}}}
	specs := BuildChildSpecs(BuildInput{Proposal: proposal, ParentIssue: 1, ParentAcceptance: []string{"done"}})
	if len(specs) != 1 || !specs[0].IsContract {
		t.Fatalf("single-phase proposal should yield one contract child, got %+v", specs)
	}
	if len(specs[0].AcceptanceCriteria) != 1 {
		t.Errorf("single contract child should carry the parent acceptance criteria")
	}
}

// TestBuildChildSpecs_BodyStatesLandabilityUnverified pins the landability
// honesty change (#2412): EVERY filed child body (contract and non-contract)
// carries the landability caveat and names the path-keyed coupling class that
// broke run 1d94db6f. This is a rendered-text change compilation cannot enforce
// and a comment-only touch would satisfy at the scope-completeness gate, so it
// is asserted behaviorally.
func TestBuildChildSpecs_BodyStatesLandabilityUnverified(t *testing.T) {
	specs := BuildChildSpecs(BuildInput{Proposal: threePhaseProposal(), ParentIssue: 2100})
	if len(specs) == 0 {
		t.Fatal("expected specs")
	}
	for i, s := range specs {
		if !strings.Contains(s.Proposal, "Landability is not verified") {
			t.Errorf("specs[%d] body should carry the landability caveat: %q", i, s.Proposal)
		}
		for _, want := range []string{"path-keyed", "surface-sweep", "rebuild matrices", "1d94db6f"} {
			if !strings.Contains(s.Proposal, want) {
				t.Errorf("specs[%d] body should name %q (the path-keyed coupling class): %q", i, want, s.Proposal)
			}
		}
	}
}

// TestBuildDoneMeans_NonContractDropsCompilesClaim pins that the non-contract
// done-means no longer asserts the unsatisfiable "compiles as an at-or-under-cap
// intermediate" claim (#2412), while the contract done-means is unchanged.
func TestBuildDoneMeans_NonContractDropsCompilesClaim(t *testing.T) {
	specs := BuildChildSpecs(BuildInput{Proposal: threePhaseProposal(), ParentIssue: 2100})
	for i, s := range specs {
		if s.IsContract {
			continue
		}
		if strings.Contains(s.DoneMeans, "compiles as an at-or-under-cap intermediate") {
			t.Errorf("non-contract specs[%d] done-means must not assert the unsatisfiable compiles claim: %q", i, s.DoneMeans)
		}
		if !strings.Contains(s.DoneMeans, "unverified") {
			t.Errorf("non-contract specs[%d] done-means should state independent landability is unverified: %q", i, s.DoneMeans)
		}
	}
}

// parentApproach builds n approach steps whose descriptions are distinctive
// (step-NN-marker) and padded to descLen runes.
func parentApproach(n, descLen int) []plan.ApproachStep {
	steps := make([]plan.ApproachStep, 0, n)
	for i := 1; i <= n; i++ {
		d := fmt.Sprintf("step-%02d-marker ", i)
		if pad := descLen - len(d); pad > 0 {
			d += strings.Repeat("x", pad)
		}
		steps = append(steps, plan.ApproachStep{Step: i, Description: d})
	}
	return steps
}

// assertEveryStepNumber fails unless block lists every step 1..n at the start
// of its own line.
func assertEveryStepNumber(t *testing.T, block string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if !strings.Contains(block, fmt.Sprintf("\n%d. ", i)) && !strings.Contains(block, fmt.Sprintf("\n%d.\n", i)) {
			t.Errorf("block is missing approach step %d at a line start", i)
		}
	}
}

func assertWithinParentPlanBound(t *testing.T, block string) {
	t.Helper()
	if got := utf8.RuneCountInString(block); got > MaxParentPlanContextChars {
		t.Errorf("parent plan context is %d characters, over the %d bound", got, MaxParentPlanContextChars)
	}
}

// TestBuildChildSpecs_CarriesParentPlanContext (C1, #5223) pins that every
// child, contract and non-contract, carries the approved parent plan's summary
// and numbered approach in plan order, after its own scope statement and before
// the landability caveat. COUNTERFACTUAL: removing the block render from
// buildProposal reddens every assertion here.
func TestBuildChildSpecs_CarriesParentPlanContext(t *testing.T) {
	steps := []plan.ApproachStep{
		{Step: 1, Description: "QUOKKA add the expanded name"},
		{Step: 2, Description: "PANGOLIN migrate\n  every   consumer"},
		{Step: 3, Description: "AXOLOTL delete the old name"},
	}
	specs := BuildChildSpecs(BuildInput{
		Proposal:       threePhaseProposal(),
		ParentIssue:    2100,
		ParentSummary:  "NARWHAL rename Foo to NewFoo across the backend",
		ParentApproach: steps,
	})
	if len(specs) != 3 {
		t.Fatalf("len(specs) = %d, want 3", len(specs))
	}
	for i, s := range specs {
		want := []string{
			ParentPlanContextHeading,
			"Summary: NARWHAL rename Foo to NewFoo across the backend",
			"1. QUOKKA add the expanded name",
			// A multi-line description is flattened onto its one step line.
			"2. PANGOLIN migrate every consumer",
			"3. AXOLOTL delete the old name",
		}
		last := -1
		for _, w := range want {
			at := strings.Index(s.Proposal, w)
			if at < 0 {
				t.Errorf("specs[%d].Proposal is missing %q:\n%s", i, w, s.Proposal)
				continue
			}
			if at < last {
				t.Errorf("specs[%d].Proposal has %q out of plan order", i, w)
			}
			last = at
		}
		scopeAt := strings.Index(s.Proposal, s.ScopeStatement)
		headingAt := strings.Index(s.Proposal, ParentPlanContextHeading)
		caveatAt := strings.Index(s.Proposal, LandabilityCaveat)
		if scopeAt < 0 || headingAt < scopeAt || caveatAt < headingAt {
			t.Errorf("specs[%d]: block must sit after the scope statement and before the caveat (scope %d, block %d, caveat %d)", i, scopeAt, headingAt, caveatAt)
		}
		if strings.Contains(s.Proposal, "truncated to fit") {
			t.Errorf("specs[%d]: a block within the bound must carry no truncation note", i)
		}
	}
}

// TestBuildChildSpecs_NoParentPlanContextOmitsBlock (C2, #5223) compares the
// Proposal against golden literals written from the pre-#5223 render, so a plan
// with no summary and no approach files byte-identical bodies. A whitespace-only
// summary counts as empty. COUNTERFACTUAL: rendering the heading unconditionally
// reddens both rows.
func TestBuildChildSpecs_NoParentPlanContextOmitsBlock(t *testing.T) {
	wantMigrate := "Phase 2 of the approved split proposal for the over-cap change tracked on #2100 (design: #2008).\n\n" +
		"Symbol-set scope: migrate consumers of Foo to NewFoo across backend/internal\n\n" +
		"This is a transitional phase of the split for #2100; it references but does not close the parent.\n\n" +
		LandabilityCaveat
	wantContract := "Phase 3 of the approved split proposal for the over-cap change tracked on #2100 (design: #2008).\n\n" +
		"Symbol-set scope: delete Foo now that all consumers use NewFoo\n\n" +
		"This is the terminal (contract) phase and the acceptance carrier for parent #2100: it carries the parent's acceptance criteria. It does NOT close the parent — GitHub auto-closes an issue only from a PR/commit and only the enclosing issue, so a close line here would be functionless. The parent is closed when this contract child lands, automated by follow-up #2062 (E50.6) once it ships.\n\n" +
		LandabilityCaveat
	for _, tc := range []struct {
		name    string
		summary string
	}{
		{"empty summary, nil approach", ""},
		{"whitespace summary, nil approach", " \n\t "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			specs := BuildChildSpecs(BuildInput{Proposal: threePhaseProposal(), ParentIssue: 2100, ParentSummary: tc.summary})
			if specs[1].Proposal != wantMigrate {
				t.Errorf("migrate Proposal changed:\n got: %q\nwant: %q", specs[1].Proposal, wantMigrate)
			}
			if specs[2].Proposal != wantContract {
				t.Errorf("contract Proposal changed:\n got: %q\nwant: %q", specs[2].Proposal, wantContract)
			}
			for i, s := range specs {
				if strings.Contains(s.Proposal, ParentPlanContextHeading) {
					t.Errorf("specs[%d] carries the context heading with nothing to show", i)
				}
			}
		})
	}
}

// TestBuildChildSpecs_ParentPlanContextBounded (C3, #5223, approval condition
// H2) pins the bound and the degrade order: the block never exceeds
// MaxParentPlanContextChars, the summary is cut before any step, every step
// number survives a step-description cut, and a plan with too many steps for
// one line each says so. COUNTERFACTUALS: removing the cap reddens every row's
// length assertion; dropping the summary-first pass reddens "long summary,
// short steps"; dropping steps instead of shortening them reddens "long steps".
func TestBuildChildSpecs_ParentPlanContextBounded(t *testing.T) {
	longSummary := strings.Repeat("summary words ", 2000) // ~28k runes
	t.Run("long summary, short steps: summary cut, every step whole", func(t *testing.T) {
		steps := parentApproach(5, 40)
		block := renderParentPlanContext(2100, longSummary, steps)
		assertWithinParentPlanBound(t, block)
		for _, st := range steps {
			if !strings.Contains(block, fmt.Sprintf("%d. %s", st.Step, st.Description)) {
				t.Errorf("step %d must be shown in full while only the summary is cut", st.Step)
			}
		}
		if !strings.Contains(block, "Summary: summary words") || !strings.Contains(block, "the summary is shortened (marked …)") {
			t.Errorf("summary should be shortened with the elision note:\n%s", block)
		}
		if !strings.HasSuffix(block, "The full approved plan is on parent #2100's Fishhawk run._") {
			t.Errorf("a truncated block must end with the note pointing at the parent run:\n%s", block[len(block)-200:])
		}
	})
	t.Run("long summary, no steps: summary cut, no approach section", func(t *testing.T) {
		block := renderParentPlanContext(2100, longSummary, nil)
		assertWithinParentPlanBound(t, block)
		if strings.Contains(block, "Approach:") || strings.Contains(block, "every approach step") {
			t.Errorf("a plan without steps must not claim an approach:\n%s", block[:300])
		}
		if !strings.Contains(block, "the summary is shortened") {
			t.Errorf("summary should be shortened:\n%s", block[len(block)-300:])
		}
	})
	t.Run("long steps: summary omitted, every step number kept, descriptions cut", func(t *testing.T) {
		steps := parentApproach(40, 400) // 16k runes of steps alone
		// One short step must survive whole: the per-step limit trims only the
		// long descriptions.
		steps[3].Description = "SHORT-STEP-FOUR"
		block := renderParentPlanContext(2100, "a summary that must go first", steps)
		assertWithinParentPlanBound(t, block)
		assertEveryStepNumber(t, block, 40)
		for i := 1; i <= 40; i++ {
			if !strings.Contains(block, fmt.Sprintf("%d. step-%02d-marker", i, i)) && i != 4 {
				t.Errorf("step %d lost its leading description text", i)
			}
		}
		if !strings.Contains(block, "\n4. SHORT-STEP-FOUR\n") {
			t.Errorf("a short step must not be cut by the per-step limit")
		}
		if strings.Contains(block, "a summary that must go first") {
			t.Errorf("the summary must be cut before any step is shortened")
		}
		if !strings.Contains(block, "x…\n") {
			t.Errorf("a shortened step must carry the elision marker")
		}
		if !strings.Contains(block, "the summary is omitted; every approach step is kept, with long step descriptions shortened (marked …)") {
			t.Errorf("note should state what was cut:\n%s", block[len(block)-400:])
		}
	})
	t.Run("too many steps for one line each: says so explicitly", func(t *testing.T) {
		steps := parentApproach(3000, 20)
		block := renderParentPlanContext(2100, "summary", steps)
		assertWithinParentPlanBound(t, block)
		if !strings.Contains(block, "The parent approach has 3000 steps, too many to list even one line per step") {
			t.Errorf("an unlistable approach must be stated explicitly:\n%s", block)
		}
		if strings.Contains(block, "Approach:") {
			t.Errorf("an unlistable approach must not render a partial list as if whole")
		}
	})
	t.Run("parent issue unknown: note names the parent issue generically", func(t *testing.T) {
		block := renderParentPlanContext(0, longSummary, nil)
		if !strings.Contains(block, "on the parent issue's Fishhawk run") || strings.Contains(block, "#0") {
			t.Errorf("parent 0 must not render #0:\n%s", block[len(block)-200:])
		}
	})
	t.Run("carried onto every child Proposal", func(t *testing.T) {
		steps := parentApproach(40, 400)
		specs := BuildChildSpecs(BuildInput{Proposal: threePhaseProposal(), ParentIssue: 2100, ParentSummary: longSummary, ParentApproach: steps})
		block := renderParentPlanContext(2100, longSummary, steps)
		for i, s := range specs {
			if !strings.Contains(s.Proposal, block+"\n\n") {
				t.Errorf("specs[%d].Proposal must carry the bounded block, ending at a line boundary", i)
			}
		}
	})
}

// TestRenderParentPlanContext_SummaryOmittedKeepsStepsWhole isolates the
// summary-OMITTED arm: the steps fit whole only once even a one-rune summary
// (plus its shortened-summary note) is dropped. It sweeps the padding across the window around the bound, so the arm
// is reached without hard-coding the heading or note lengths, and asserts the
// bound and the full step list at every width. COUNTERFACTUAL: deleting the
// summary-omitted pass sends those widths to the step-shortening pass, which
// cuts the long step and reddens the whole-step assertion.
func TestRenderParentPlanContext_SummaryOmittedKeepsStepsWhole(t *testing.T) {
	hits := 0
	for pad := MaxParentPlanContextChars - 700; pad <= MaxParentPlanContextChars; pad++ {
		steps := []plan.ApproachStep{
			{Step: 1, Description: "first"},
			{Step: 2, Description: strings.Repeat("y", pad)},
		}
		// The summary must be longer than the truncation note: a shorter one
		// would fit whole wherever its omission fits, masking the arm.
		block := renderParentPlanContext(2100, strings.Repeat("s", 1000), steps)
		assertWithinParentPlanBound(t, block)
		assertEveryStepNumber(t, block, 2)
		if strings.Contains(block, "the summary is omitted; every approach step is shown in full") {
			hits++
			if !strings.Contains(block, "2. "+steps[1].Description+"\n") {
				t.Fatalf("pad %d: the summary-omitted arm must keep every step whole", pad)
			}
		}
	}
	if hits == 0 {
		t.Fatal("no width reached the summary-omitted arm; the fixture no longer isolates it")
	}
}

// TestRenderParentPlanContext_StepNumberFallback pins that a step's own number
// is rendered and an unset (zero) number falls back to its 1-based position,
// and that a blank description renders the bare number.
func TestRenderParentPlanContext_StepNumberFallback(t *testing.T) {
	block := renderParentPlanContext(2100, "", []plan.ApproachStep{
		{Step: 7, Description: "seventh"},
		{Step: 0, Description: "positional"},
		{Step: 0, Description: "   "},
	})
	for _, want := range []string{"\n7. seventh\n", "\n2. positional\n", "\n3."} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "Summary:") {
		t.Errorf("a blank summary must not render a Summary line")
	}
}

func TestLargestFitting(t *testing.T) {
	under := func(limit int) func(int) bool { return func(n int) bool { return n <= limit } }
	for _, tc := range []struct {
		name         string
		lo, hi, fits int
		want         int
		wantOK       bool
	}{
		{"largest in range", 0, 100, 37, 37, true},
		{"all fit", 0, 10, 99, 10, true},
		{"only lo fits", 5, 10, 5, 5, true},
		{"lo does not fit", 5, 10, 4, 0, false},
		{"empty range", 3, 2, 99, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := largestFitting(tc.lo, tc.hi, under(tc.fits))
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("largestFitting(%d, %d) = (%d, %v), want (%d, %v)", tc.lo, tc.hi, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
