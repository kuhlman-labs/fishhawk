package splitfiling

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// contractEvidence builds a reachability evidence slice whose contract phase
// (last index of a 3-phase proposal, index 2) carries the given derived count.
func contractEvidence(derived, declared int) []PhaseEvidence {
	return []PhaseEvidence{
		{Index: 0, Title: "expand", DeclaredCount: 3, DerivedCount: 3},
		{Index: 1, Title: "migrate", DeclaredCount: 4, DerivedCount: 4},
		{Index: 2, Title: "contract: delete the transitional names", DeclaredCount: declared, DerivedCount: derived},
	}
}

func TestClassify_DeleteOnlyByDefault(t *testing.T) {
	// Contract phase derived (5) is at-or-under the cap (10) → delete-only.
	got := Classify(threePhaseProposal(), contractEvidence(5, 5), 10)
	if got != ClassificationDeleteOnly {
		t.Errorf("Classify = %q, want %q", got, ClassificationDeleteOnly)
	}
}

func TestClassify_GovernedExceptionWhenDerivedExceedsCap(t *testing.T) {
	// Contract phase derived (25) exceeds the cap (10) → governed-exception.
	got := Classify(threePhaseProposal(), contractEvidence(25, 12), 10)
	if got != ClassificationGovernedException {
		t.Errorf("Classify = %q, want %q", got, ClassificationGovernedException)
	}
}

func TestClassify_DerivedEqualToCapIsDeleteOnly(t *testing.T) {
	// Equal, not strictly greater → the rename still fits one in-cap PR.
	got := Classify(threePhaseProposal(), contractEvidence(10, 10), 10)
	if got != ClassificationDeleteOnly {
		t.Errorf("Classify = %q, want %q (equal is not over-cap)", got, ClassificationDeleteOnly)
	}
}

func TestClassify_FailSafeDeleteOnlyWhenEvidenceMissing(t *testing.T) {
	// No evidence at all → fail safe to delete-only.
	if got := Classify(threePhaseProposal(), nil, 10); got != ClassificationDeleteOnly {
		t.Errorf("Classify(nil evidence) = %q, want %q", got, ClassificationDeleteOnly)
	}
	// Evidence present for other phases but not the contract index → fail safe.
	partial := []PhaseEvidence{{Index: 0, DerivedCount: 99}, {Index: 1, DerivedCount: 99}}
	if got := Classify(threePhaseProposal(), partial, 10); got != ClassificationDeleteOnly {
		t.Errorf("Classify(no contract evidence) = %q, want %q", got, ClassificationDeleteOnly)
	}
}

func TestClassify_FailSafeDeleteOnlyWhenCapUnresolved(t *testing.T) {
	// Even a huge derived count fails safe to delete-only when the cap is
	// unresolved (<= 0), because "over-cap" is undecidable.
	for _, cap := range []int{0, -1} {
		if got := Classify(threePhaseProposal(), contractEvidence(999, 1), cap); got != ClassificationDeleteOnly {
			t.Errorf("Classify(cap=%d) = %q, want %q", cap, got, ClassificationDeleteOnly)
		}
	}
}

func TestClassify_EmptyProposalIsDeleteOnly(t *testing.T) {
	if got := Classify(plan.SplitProposal{}, contractEvidence(999, 1), 10); got != ClassificationDeleteOnly {
		t.Errorf("Classify(empty proposal) = %q, want %q", got, ClassificationDeleteOnly)
	}
}

func TestDraftCapException_GovernedRaisesCapAndStatesGovernance(t *testing.T) {
	const cap, derived, declared = 10, 25, 12
	draft := DraftCapException(threePhaseProposal(), contractEvidence(derived, declared), cap)
	if draft == nil {
		t.Fatal("governed-exception should produce a draft")
	}
	// The spec diff literally raises max_files_changed from cap to derived.
	if !strings.Contains(draft.SpecDiff, fmt.Sprintf("-        max_files_changed: %d", cap)) {
		t.Errorf("spec diff should remove the old cap %d:\n%s", cap, draft.SpecDiff)
	}
	if !strings.Contains(draft.SpecDiff, fmt.Sprintf("+        max_files_changed: %d", derived)) {
		t.Errorf("spec diff should raise the cap to derived %d:\n%s", derived, draft.SpecDiff)
	}
	// The PR body names the atomicity evidence and states the governance posture.
	body := draft.PRBody
	if !strings.Contains(body, fmt.Sprintf("%d files", derived)) {
		t.Errorf("PR body should name the derived count %d:\n%s", derived, body)
	}
	if !strings.Contains(body, fmt.Sprintf("%d declared", declared)) {
		t.Errorf("PR body should name the declared count %d:\n%s", declared, body)
	}
	if !strings.Contains(body, "Operator-authored, admin-merged") {
		t.Errorf("PR body must state operator-authored + admin-merged:\n%s", body)
	}
	if !strings.Contains(body, "agent-forbidden") {
		t.Errorf("PR body must explain .fishhawk/** is agent-forbidden:\n%s", body)
	}
}

func TestDraftCapException_DeleteOnlyReturnsNoDraft(t *testing.T) {
	if got := DraftCapException(threePhaseProposal(), contractEvidence(5, 5), 10); got != nil {
		t.Errorf("delete-only should yield no draft, got %+v", got)
	}
	// Fail-safe branches also yield no draft.
	if got := DraftCapException(threePhaseProposal(), nil, 10); got != nil {
		t.Errorf("missing evidence should yield no draft, got %+v", got)
	}
	if got := DraftCapException(threePhaseProposal(), contractEvidence(999, 1), 0); got != nil {
		t.Errorf("unresolved cap should yield no draft, got %+v", got)
	}
}

func TestDraftCapException_PRBodyFallsBackToPhaseIndexTitle(t *testing.T) {
	// A contract evidence entry with an empty title falls back to "phase N".
	ev := []PhaseEvidence{
		{Index: 0, DerivedCount: 3},
		{Index: 1, DerivedCount: 4},
		{Index: 2, Title: "", DeclaredCount: 12, DerivedCount: 25},
	}
	draft := DraftCapException(threePhaseProposal(), ev, 10)
	if draft == nil {
		t.Fatal("governed-exception should produce a draft")
	}
	if !strings.Contains(draft.PRBody, "phase 3") {
		t.Errorf("PR body should fall back to 'phase 3' for an empty title:\n%s", draft.PRBody)
	}
}

// --- PhaseCapViolations (#2412) ---

// phaseWithNFiles builds one split phase whose scope declares n files.
func phaseWithNFiles(title string, n int) plan.SplitPhase {
	files := make([]plan.ScopeFile, n)
	for i := range files {
		files[i] = plan.ScopeFile{Path: fmt.Sprintf("%s/f%d.go", title, i), Operation: plan.FileOpModify}
	}
	return plan.SplitPhase{Title: title, Scope: &plan.Scope{Files: files}}
}

func TestPhaseCapViolations_Table(t *testing.T) {
	for _, tc := range []struct {
		name     string
		proposal plan.SplitProposal
		cap      int
		want     []PhaseCapViolation
	}{
		{
			name: "all under cap",
			proposal: plan.SplitProposal{Phases: []plan.SplitPhase{
				phaseWithNFiles("expand", 3), phaseWithNFiles("migrate", 4),
			}},
			cap:  10,
			want: nil,
		},
		{
			name: "phase at cap is not a violation",
			proposal: plan.SplitProposal{Phases: []plan.SplitPhase{
				phaseWithNFiles("expand", 10),
			}},
			cap:  10,
			want: nil,
		},
		{
			name: "single over-cap phase",
			proposal: plan.SplitProposal{Phases: []plan.SplitPhase{
				phaseWithNFiles("expand", 3), phaseWithNFiles("lead", 12),
			}},
			cap:  10,
			want: []PhaseCapViolation{{Index: 1, Title: "lead", DeclaredCount: 12, Cap: 10}},
		},
		{
			name: "multiple violators returned in phase order",
			proposal: plan.SplitProposal{Phases: []plan.SplitPhase{
				phaseWithNFiles("a", 20), phaseWithNFiles("b", 5), phaseWithNFiles("c", 11),
			}},
			cap: 10,
			want: []PhaseCapViolation{
				{Index: 0, Title: "a", DeclaredCount: 20, Cap: 10},
				{Index: 2, Title: "c", DeclaredCount: 11, Cap: 10},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PhaseCapViolations(tc.proposal, tc.cap)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d violations, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("violation[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestPhaseCapViolations_UnresolvedCapFailsOpen pins the cap <= 0 fail-open
// guard (#2412): an unresolved cap must NEVER manufacture a violation, even for
// a phase declaring many files. This is the counterfactual vehicle for the
// fail-open guard — deleting the `if cap <= 0 { return nil }` line makes the
// 999-file phase produce a violation against cap 0.
func TestPhaseCapViolations_UnresolvedCapFailsOpen(t *testing.T) {
	proposal := plan.SplitProposal{Phases: []plan.SplitPhase{phaseWithNFiles("huge", 999)}}
	for _, cap := range []int{0, -1} {
		if got := PhaseCapViolations(proposal, cap); got != nil {
			t.Errorf("PhaseCapViolations(cap=%d) = %+v, want nil (unresolved cap never manufactures a violation)", cap, got)
		}
	}
}

// TestPhaseCapViolations_NilScopeSkipped confirms a nil/empty phase scope is
// skipped defensively rather than panicking (checkSplitProposal already rejects
// it on a validated plan, but the pure function must not assume that).
func TestPhaseCapViolations_NilScopeSkipped(t *testing.T) {
	proposal := plan.SplitProposal{Phases: []plan.SplitPhase{
		{Title: "nil-scope", Scope: nil},
		{Title: "empty-scope", Scope: &plan.Scope{}},
		phaseWithNFiles("over", 12),
	}}
	got := PhaseCapViolations(proposal, 10)
	if len(got) != 1 || got[0].Title != "over" {
		t.Fatalf("want only the 'over' phase flagged, got %+v", got)
	}
}

// TestPhaseCapViolation_StringSharedPhrasing pins the ONE shared rendering used
// by the plan-gate advisory, the refusal payload, and the parent comment.
func TestPhaseCapViolation_StringSharedPhrasing(t *testing.T) {
	v := PhaseCapViolation{Index: 1, Title: "lead", DeclaredCount: 12, Cap: 10}
	s := v.String()
	for _, want := range []string{"phase 2", `"lead"`, "12 files", "cap of 10", "irreducible"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, want it to contain %q", s, want)
		}
	}
}
