package intakegroom

import (
	"errors"
	"strings"
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

func TestParseSourceRefs_AcceptsHashOrBareNumbersAndDedupes(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs []string
		want []int
	}{
		{"nil list", nil, nil},
		{"empty list", []string{}, nil},
		{"hash form", []string{"#12"}, []int{12}},
		{"bare form", []string{"12"}, []int{12}},
		{"surrounding whitespace is trimmed", []string{" #12 ", "\t7\n"}, []int{12, 7}},
		{"duplicates dedupe in first-seen order", []string{"#12", "7", "12", "#7", "3"}, []int{12, 7, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSourceRefs(tc.refs)
			if err != nil {
				t.Fatalf("ParseSourceRefs(%q) error = %v, want nil", tc.refs, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseSourceRefs(%q) = %v, want %v", tc.refs, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("ParseSourceRefs(%q) = %v, want %v", tc.refs, got, tc.want)
				}
			}
		})
	}
}

// TestParseSourceRefs_RejectsMalformedRefs pins that every malformed ref fails
// the WHOLE list with a nil result and a *SourceRefError naming that ref —
// including when it follows well-formed refs, so a caller cannot receive a
// partially-parsed list it might mistake for the whole declaration.
func TestParseSourceRefs_RejectsMalformedRefs(t *testing.T) {
	for _, bad := range []string{"", "   ", "#", "#0", "0", "-3", "#-3", "+3", "abc", "#12a", "o/r#1", "kuhlman-labs/fishhawk#12", "##12", "1 2", "99999999999999999999"} {
		t.Run(bad, func(t *testing.T) {
			got, err := ParseSourceRefs([]string{"#5", bad})
			if err == nil {
				t.Fatalf("ParseSourceRefs accepted malformed ref %q as %v", bad, got)
			}
			if got != nil {
				t.Fatalf("a malformed list must yield nil numbers, got %v", got)
			}
			var refErr *SourceRefError
			if !errors.As(err, &refErr) || refErr.Ref != bad {
				t.Fatalf("error = %#v, want *SourceRefError naming %q", err, bad)
			}
			if !strings.Contains(err.Error(), "'#N'") {
				t.Errorf("error message does not say what a ref must look like: %v", err)
			}
		})
	}
}

func TestDerivesFromWindow_ResolvesInWindowAndReportsOutOfWindowNumberOnly(t *testing.T) {
	f := Filing{Title: "anything", SourceNumbers: []int{1234, 77, 9}}
	candidates := []Candidate{
		{Number: 9, Title: "a closed source", URL: "u/9", Closed: true},
		{Number: 1234, Title: "[E22.4] Add the widget endpoint", URL: "u/1234"},
		{Number: 500, Title: "not a source"},
	}

	got := DerivesFromWindow(f, candidates)

	want := []SourceItem{
		{Number: 1234, Title: "[E22.4] Add the widget endpoint", URL: "u/1234", InWindow: true},
		{Number: 77},
		{Number: 9, Title: "a closed source", URL: "u/9", Closed: true, InWindow: true},
	}
	if len(got) != len(want) {
		t.Fatalf("DerivesFromWindow = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A nil window — the degraded-scan path — still reports the provenance,
	// number-only and out-of-window.
	degraded := DerivesFromWindow(f, nil)
	if len(degraded) != 3 {
		t.Fatalf("nil window = %+v, want three number-only entries", degraded)
	}
	for _, d := range degraded {
		if d.InWindow || d.Title != "" || d.URL != "" || d.Closed {
			t.Errorf("a nil window resolved %+v; want number-only", d)
		}
	}

	if got := DerivesFromWindow(Filing{Title: "no sources"}, candidates); got != nil {
		t.Errorf("no declared sources must yield nil, got %+v", got)
	}
}

// TestDerivesFromWindow_EstablishesTheMarkerInvariantsByConstruction is the
// vehicle for the writer half of the derives_from writer/validator agreement:
// DerivesFromWindow must never emit an entry validSignals would reject, even
// when handed a Filing that did not come through ParseSourceRefs. The bad
// numbers are seeded BY CONSTRUCTION in the Filing literal.
func TestDerivesFromWindow_EstablishesTheMarkerInvariantsByConstruction(t *testing.T) {
	f := Filing{Title: "anything", SourceNumbers: []int{0, 5, -2, 5, 8}}

	got := DerivesFromWindow(f, nil)

	if len(got) != 2 || got[0].Number != 5 || got[1].Number != 8 {
		t.Fatalf("DerivesFromWindow = %+v, want exactly #5 then #8 (non-positive dropped, repeats deduped)", got)
	}
	if !validSignals(Signals{Score: Score{Unscored: true, CharterGap: "g"}, DerivesFrom: got}) {
		t.Fatalf("validSignals rejected what DerivesFromWindow wrote: %+v", got)
	}
	if got := DerivesFromWindow(Filing{SourceNumbers: []int{0, -1}}, nil); got != nil {
		t.Fatalf("only non-positive numbers must yield nil, got %+v", got)
	}
}

func TestEvaluate_ExcludesSourcesFromDuplicatesAndReportsDerivesFrom(t *testing.T) {
	f := Filing{
		Title:         "[E81.9] Intake preview source exclusion",
		Body:          "a draft written from #1234",
		Type:          "feature",
		Labels:        []string{"type:feature"},
		SourceNumbers: []int{1234},
	}
	candidates := []Candidate{
		{Number: 1234, Title: "[E81.4] Intake preview source exclusion", Body: "the source item's own body", URL: "u/1234"},
		{Number: 1240, Title: "Intake preview source exclusion work", URL: "u/1240"},
	}

	got := Evaluate(f, candidates, testCharter())

	for _, d := range got.Duplicates {
		if d.Number == 1234 {
			t.Fatalf("the declared source #1234 was reported as a duplicate: %+v", got.Duplicates)
		}
	}
	if len(got.Duplicates) == 0 || got.Duplicates[0].Number != 1240 {
		t.Fatalf("the unrelated near-duplicate #1240 must still be reported, got %+v", got.Duplicates)
	}
	if len(got.DerivesFrom) != 1 || got.DerivesFrom[0].Number != 1234 || !got.DerivesFrom[0].InWindow {
		t.Fatalf("DerivesFrom = %+v, want #1234 in_window", got.DerivesFrom)
	}
}

func TestSignals_HasFindingsIgnoresDerivesFrom(t *testing.T) {
	s := Signals{
		DerivesFrom: []SourceItem{{Number: 1234, InWindow: true, Title: "t"}},
		Score:       Score{Unscored: true, CharterGap: "nothing fired"},
	}
	if s.HasFindings() {
		t.Fatal("derives_from is provenance, not a finding — counting it would render a degraded filing's body")
	}
}
