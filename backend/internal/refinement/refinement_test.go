package refinement

import (
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
)

// validChild returns a minimal conventions-complete child for composing test
// drafts. Callers mutate the returned copy.
func validChild(summary string) ChildDraft {
	return ChildDraft{
		Summary:            summary,
		Proposal:           "do the thing",
		DoneMeans:          "the thing is done",
		AcceptanceCriteria: []string{"it works"},
		Labels:             []string{"area:backend", "autonomy:medium"},
	}
}

// validDraft returns a two-child draft that passes Validate, for mutation in
// the failure-mode table.
func validDraft() EpicDraft {
	return EpicDraft{
		Epic: EpicSpec{Summary: "stand up X", Scope: "X and its wiring", OutOfScope: "Y"},
		Children: []ChildDraft{
			validChild("child one"),
			validChild("child two"),
		},
	}
}

func TestValidate_HappyPath(t *testing.T) {
	d := validDraft()
	// A satisfiable dependency edge: child two (ordinal 2) depends on child
	// one (ordinal 1).
	d.Children[1].DependsOn = []int{1}
	if err := d.Validate(); err != nil {
		t.Fatalf("Validate on a well-formed draft: %v", err)
	}
}

func TestValidate_DanglingDependencyNamesEdge(t *testing.T) {
	d := validDraft()
	// Child one depends on ordinal 5, which is out of range for a 2-child
	// draft — a dangling edge.
	d.Children[0].DependsOn = []int{5}
	err := d.Validate()
	if err == nil {
		t.Fatal("Validate accepted an out-of-range depends_on ordinal")
	}
	if !errors.Is(err, campaign.ErrDanglingDependency) {
		t.Fatalf("error = %v, want wrapped campaign.ErrDanglingDependency", err)
	}
	// The wrapped assembler message names the offending edge in the campaign
	// ref convention (issue:1->issue:5).
	if !strings.Contains(err.Error(), "issue:1->issue:5") {
		t.Errorf("error %q does not name the dangling edge issue:1->issue:5", err.Error())
	}
}

func TestValidate_CycleRejected(t *testing.T) {
	d := validDraft()
	// child one <-> child two mutual dependency: a 2-node cycle.
	d.Children[0].DependsOn = []int{2}
	d.Children[1].DependsOn = []int{1}
	err := d.Validate()
	if err == nil {
		t.Fatal("Validate accepted a cyclic dependency set")
	}
	if !errors.Is(err, campaign.ErrCycle) {
		t.Fatalf("error = %v, want wrapped campaign.ErrCycle", err)
	}
}

func TestValidate_SelfEdgeRejected(t *testing.T) {
	d := validDraft()
	// child one depends on itself (ordinal 1) — a length-1 cycle.
	d.Children[0].DependsOn = []int{1}
	err := d.Validate()
	if err == nil {
		t.Fatal("Validate accepted a self-referential depends_on ordinal")
	}
	if !errors.Is(err, campaign.ErrCycle) {
		t.Fatalf("error = %v, want wrapped campaign.ErrCycle for a self-edge", err)
	}
}

func TestValidate_ChildWithoutAcceptanceCriteriaNamesChild(t *testing.T) {
	d := validDraft()
	d.Children[1].AcceptanceCriteria = nil
	err := d.Validate()
	if err == nil {
		t.Fatal("Validate accepted a child with zero acceptance criteria")
	}
	// Named by ordinal (2) and summary so the operator can find the child.
	if !strings.Contains(err.Error(), "child 2") || !strings.Contains(err.Error(), "child two") {
		t.Errorf("error %q does not name the criteria-less child (ordinal 2, %q)", err.Error(), "child two")
	}
}

func TestValidate_ZeroChildrenRejected(t *testing.T) {
	d := validDraft()
	d.Children = nil
	err := d.Validate()
	if err == nil {
		t.Fatal("Validate accepted a draft with zero children")
	}
	if !strings.Contains(err.Error(), "at least one child") {
		t.Errorf("error %q does not explain the zero-children rule", err.Error())
	}
}

func TestValidate_EmptyEpicSummaryRejected(t *testing.T) {
	d := validDraft()
	d.Epic.Summary = "  "
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "epic summary") {
		t.Fatalf("Validate on empty epic summary = %v, want epic-summary error", err)
	}
}

func TestValidate_EmptyEpicScopeRejected(t *testing.T) {
	d := validDraft()
	d.Epic.Scope = ""
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "epic scope") {
		t.Fatalf("Validate on empty epic scope = %v, want epic-scope error", err)
	}
}

func TestValidate_EmptyChildSummaryRejected(t *testing.T) {
	d := validDraft()
	d.Children[0].Summary = ""
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "child 1") {
		t.Fatalf("Validate on empty child summary = %v, want child-1 error", err)
	}
}

func TestValidate_EmptyChildProposalRejected(t *testing.T) {
	d := validDraft()
	d.Children[0].Proposal = ""
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "proposal") {
		t.Fatalf("Validate on empty child proposal = %v, want proposal error", err)
	}
}

// childN returns a draft of n independent conventions-complete children (no
// edges), for composing Waves() shape tests.
func childN(n int) EpicDraft {
	d := EpicDraft{Epic: EpicSpec{Summary: "e", Scope: "s"}}
	for i := 0; i < n; i++ {
		d.Children = append(d.Children, validChild("child"))
	}
	return d
}

func TestWaves_LinearChain(t *testing.T) {
	// 1 <- 2 <- 3: each wave holds exactly the next ordinal.
	d := childN(3)
	d.Children[1].DependsOn = []int{1}
	d.Children[2].DependsOn = []int{2}
	waves, err := d.Waves()
	if err != nil {
		t.Fatalf("Waves: %v", err)
	}
	want := [][]int{{1}, {2}, {3}}
	if !equalWaves(waves, want) {
		t.Errorf("linear-chain waves = %v, want %v", waves, want)
	}
}

func TestWaves_Diamond(t *testing.T) {
	// 1 <- {2,3} <- 4: three waves, the middle holding two ordinals.
	d := childN(4)
	d.Children[1].DependsOn = []int{1}
	d.Children[2].DependsOn = []int{1}
	d.Children[3].DependsOn = []int{2, 3}
	waves, err := d.Waves()
	if err != nil {
		t.Fatalf("Waves: %v", err)
	}
	want := [][]int{{1}, {2, 3}, {4}}
	if !equalWaves(waves, want) {
		t.Errorf("diamond waves = %v, want %v", waves, want)
	}
}

func TestWaves_IndependentChildren(t *testing.T) {
	// No edges: every child dispatches in wave 0.
	d := childN(3)
	waves, err := d.Waves()
	if err != nil {
		t.Fatalf("Waves: %v", err)
	}
	want := [][]int{{1, 2, 3}}
	if !equalWaves(waves, want) {
		t.Errorf("independent-children waves = %v, want %v", waves, want)
	}
}

func TestWaves_CyclicFailsClosed(t *testing.T) {
	d := childN(2)
	d.Children[0].DependsOn = []int{2}
	d.Children[1].DependsOn = []int{1}
	if _, err := d.Waves(); !errors.Is(err, campaign.ErrCycle) {
		t.Fatalf("Waves on a cyclic graph = %v, want wrapped campaign.ErrCycle", err)
	}
}

func TestWaves_DanglingFailsClosed(t *testing.T) {
	d := childN(2)
	d.Children[0].DependsOn = []int{5}
	if _, err := d.Waves(); !errors.Is(err, campaign.ErrDanglingDependency) {
		t.Fatalf("Waves on a dangling edge = %v, want wrapped campaign.ErrDanglingDependency", err)
	}
}

// equalWaves compares two wave slices for exact ordinal equality (order within
// a wave is deterministic — ascending index — from campaign.Assemble).
func equalWaves(a, b [][]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}
