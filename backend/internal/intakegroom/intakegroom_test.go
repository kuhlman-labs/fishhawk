package intakegroom

import (
	"testing"
)

// rubricFixture is a minimal charter carrying exactly the three ids the
// structural rules cite, in the shipped table shape.
const rubricFixture = `## 4. Prioritization rubric

| id | line |
|---|---|
| **S2** | Superseded or duplicated by another item. |
| **S4** | Missing the structure the loop needs. |
| **U4** | Blocks nothing, and nothing blocks it. |
`

// charterPath is the declared charter path every fixture uses. It is shared so
// a gap string that names the path can be asserted against the same literal.
const charterPath = ".fishhawk/charter.md"

func testCharter() Charter {
	return Charter{
		Path:        charterPath,
		ContentHash: "sha256:test",
		RubricIDs:   ParseRubricIDs(rubricFixture),
		Resolved:    true,
	}
}

func TestEvaluate_HealthyFilingDerivesAllThreeSignals(t *testing.T) {
	f := Filing{
		Title:  "[E54.9] Backlog grooming intake hook duplicate detection",
		Type:   "feature",
		Labels: []string{"type:feature", "area:backend"},
	}
	candidates := []Candidate{
		{Number: 100, Title: "[E54.7] Backlog grooming intake hook duplicate detection", Labels: []string{"type:feature"}, URL: "u/100"},
		{Number: 54, Title: "[E54] Backlog grooming", Labels: []string{"type:epic", "area:backend"}, URL: "u/54"},
		{Number: 900, Title: "Unrelated helm chart ingress work", URL: "u/900"},
	}

	got := Evaluate(f, candidates, testCharter())

	if got.Degraded {
		t.Fatalf("Evaluate degraded unexpectedly: %q", got.DegradeReason)
	}
	if len(got.Duplicates) == 0 || got.Duplicates[0].Number != 100 {
		t.Fatalf("want #100 as top duplicate, got %+v", got.Duplicates)
	}
	if got.EpicSuggestion == nil || got.EpicSuggestion.Number != 54 {
		t.Fatalf("want #54 epic suggestion, got %+v", got.EpicSuggestion)
	}
	if got.Score.Unscored {
		t.Fatalf("want a scored result, got unscored: %s", got.Score.CharterGap)
	}
	if got.ScannedItems != 3 {
		t.Fatalf("ScannedItems = %d, want 3", got.ScannedItems)
	}
	// The caller owns these two; Evaluate must not invent them.
	if got.WindowTruncated || got.DurationMS != 0 {
		t.Fatalf("Evaluate set caller-owned fields: truncated=%v duration=%d", got.WindowTruncated, got.DurationMS)
	}
}

// TestEvaluate_EmptyRubricDegradeReasonNamesTheReadNotTheParser is the pure
// half of the #2827 reason split. An empty rubric used to collapse onto
// charter_rubric_unparsed whatever its cause, so a charter that was never
// fetched reported a PARSE failure. The reason now follows Charter.Resolved.
//
// The bad state is seeded BY CONSTRUCTION — two Charter literals differing only
// in Resolved — rather than by calling the control, so the RED under a deleted
// mapping lands on the behavioural assertion and not on fixture setup.
func TestEvaluate_EmptyRubricDegradeReasonNamesTheReadNotTheParser(t *testing.T) {
	for _, tc := range []struct {
		name    string
		charter Charter
		want    DegradeReason
	}{
		{
			name:    "never read degrades as charter_unresolved",
			charter: Charter{Path: charterPath},
			want:    DegradeReasonCharterUnresolved,
		},
		{
			name:    "read but carrying no rubric degrades as charter_rubric_unparsed",
			charter: Charter{Path: charterPath, Resolved: true},
			want:    DegradeReasonCharterRubricUnparsed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(Filing{Title: "some new work item"}, nil, tc.charter)

			if !got.Degraded {
				t.Fatal("want Degraded with an empty rubric")
			}
			if got.DegradeReason != tc.want {
				t.Fatalf("DegradeReason = %q, want %q — the reason must name the read that actually failed", got.DegradeReason, tc.want)
			}
		})
	}
}

func TestEvaluate_SkipsEpicSuggestionWhenParentDeclared(t *testing.T) {
	candidates := []Candidate{{Number: 54, Title: "[E54] Backlog grooming", Labels: []string{"type:epic"}}}

	withParent := Evaluate(Filing{Title: "Backlog grooming intake hook", ParentEpicRef: "#54"}, candidates, testCharter())
	if withParent.EpicSuggestion != nil {
		t.Fatalf("want no suggestion when a parent is declared, got %+v", withParent.EpicSuggestion)
	}

	withoutParent := Evaluate(Filing{Title: "Backlog grooming intake hook"}, candidates, testCharter())
	if withoutParent.EpicSuggestion == nil {
		t.Fatal("want a suggestion when no parent is declared")
	}
}

func TestSignals_HasFindingsExcludesUnscored(t *testing.T) {
	unscoredOnly := Signals{Score: Score{Unscored: true, CharterGap: "nothing fired"}}
	if unscoredOnly.HasFindings() {
		t.Fatal("Unscored alone must not count as a finding — it is what keeps a degraded body byte-identical")
	}
	for name, s := range map[string]Signals{
		"duplicate": {Duplicates: []DuplicateCandidate{{Number: 1}}},
		"epic":      {EpicSuggestion: &EpicSuggestion{Number: 1}},
		"citation":  {Score: Score{Citations: []Citation{{RubricID: "S4"}}}},
	} {
		if !s.HasFindings() {
			t.Errorf("%s: want HasFindings true", name)
		}
	}
}

func TestDegrade_CarriesOnlyTheReason(t *testing.T) {
	got := Degrade(DegradeReasonHookPanic)
	if !got.Degraded || got.DegradeReason != DegradeReasonHookPanic {
		t.Fatalf("Degrade = %+v", got)
	}
	if got.HasFindings() || got.ScannedItems != 0 {
		t.Fatalf("Degrade must carry no findings, got %+v", got)
	}
}

func TestDegradeReasons_ClosedSetIsStableAndUnique(t *testing.T) {
	reasons := DegradeReasons()
	if len(reasons) != 8 {
		t.Fatalf("want 8 reasons, got %d — a new reason needs a documented surface and a test", len(reasons))
	}
	seen := map[DegradeReason]bool{}
	for _, r := range reasons {
		if r == "" {
			t.Fatal("empty reason in the closed set")
		}
		if seen[r] {
			t.Fatalf("duplicate reason %q", r)
		}
		seen[r] = true
	}
}

func TestConfidence_AtLeastMedium(t *testing.T) {
	for c, want := range map[Confidence]bool{
		ConfidenceHigh:   true,
		ConfidenceMedium: true,
		ConfidenceLow:    false,
		Confidence(""):   false,
	} {
		if got := c.AtLeastMedium(); got != want {
			t.Errorf("%q.AtLeastMedium() = %v, want %v", c, got, want)
		}
	}
}
