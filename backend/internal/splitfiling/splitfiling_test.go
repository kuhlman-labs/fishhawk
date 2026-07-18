package splitfiling

import (
	"strings"
	"testing"

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
