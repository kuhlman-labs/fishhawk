package intakegroom

import (
	"os"
	"strings"
	"testing"
)

// repoCharterPath is this repository's own charter, read as the parser's
// pin against the SHIPPED table shape rather than against a fixture that
// could drift away from it.
const repoCharterPath = "../../../.fishhawk/charter.md"

func TestParseRubricIDs_ParsesTheShippedCharterShape(t *testing.T) {
	raw, err := os.ReadFile(repoCharterPath)
	if err != nil {
		t.Skipf("repository charter not readable from this checkout: %v", err)
	}
	r := ParseRubricIDs(string(raw))

	if r.Len() < 10 {
		t.Fatalf("parsed only %d rubric lines from the shipped charter: %v", r.Len(), r.IDs())
	}
	for _, id := range []string{"V1", "R2", "S2", "S4", "U4"} {
		if !r.Has(id) {
			t.Errorf("shipped charter declares %s but the parser missed it (parsed %v)", id, r.IDs())
		}
		if strings.TrimSpace(r.Quote(id)) == "" {
			t.Errorf("%s parsed with an empty quote", id)
		}
	}
	// The charter's own prose says "S4" and "V1" in running text; only table
	// rows are rubric lines.
	if r.Has("id") {
		t.Error("the table header row must not parse as a rubric line")
	}
}

func TestParseRubricIDs_NoTableYieldsAnEmptyRubric(t *testing.T) {
	r := ParseRubricIDs("# Charter\n\nProse only. Rank on V1 and S4 as you see fit.\n")
	if r.Len() != 0 {
		t.Fatalf("want an empty rubric, got %v", r.IDs())
	}
	if r.Has("V1") || r.Quote("V1") != "" {
		t.Fatal("an empty rubric must declare nothing")
	}
}

func TestParseRubricIDs_ZeroValueRubricIsSafe(t *testing.T) {
	var r Rubric
	if r.Len() != 0 || r.Has("S4") || r.Quote("S4") != "" || len(r.IDs()) != 0 {
		t.Fatal("the zero Rubric must be empty and safe to query")
	}
}

func TestParseRubricIDs_KeepsCharterOrderAndFirstDefinition(t *testing.T) {
	r := ParseRubricIDs("| **U4** | first u4 |\n| **S2** | s2 line |\n| **U4** | a later duplicate |\n")
	ids := r.IDs()
	if len(ids) != 2 || ids[0] != "U4" || ids[1] != "S2" {
		t.Fatalf("IDs = %v, want charter order [U4 S2]", ids)
	}
	if r.Quote("U4") != "first u4" {
		t.Fatalf("Quote(U4) = %q, want the first definition", r.Quote("U4"))
	}
}

func TestScoreFiling_EachStructuralRuleFiresAndDoesNot(t *testing.T) {
	rubric := ParseRubricIDs(rubricFixture)
	mediumDup := []DuplicateCandidate{{Number: 11, Confidence: ConfidenceMedium}}
	lowDup := []DuplicateCandidate{{Number: 11, Confidence: ConfidenceLow}}

	tests := []struct {
		name     string
		filing   Filing
		dups     []DuplicateCandidate
		wantCite []string
	}{
		{
			name:     "S2 fires on a medium duplicate",
			filing:   Filing{ParentEpicRef: "#54", DependsOn: []string{"#1"}},
			dups:     mediumDup,
			wantCite: []string{"S2"},
		},
		{
			name:     "S2 does not fire on a low duplicate",
			filing:   Filing{ParentEpicRef: "#54", DependsOn: []string{"#1"}},
			dups:     lowDup,
			wantCite: nil,
		},
		{
			name:     "S4 fires on a missing parent epic",
			filing:   Filing{DependsOn: []string{"#1"}},
			wantCite: []string{"S4"},
		},
		{
			name:     "S4 fires on a missing label namespace",
			filing:   Filing{ParentEpicRef: "#54", DependsOn: []string{"#1"}, MissingLabelNamespaces: []string{"phase"}},
			wantCite: []string{"S4"},
		},
		{
			name:     "S4 does not fire on a structurally complete filing",
			filing:   Filing{ParentEpicRef: "#54", DependsOn: []string{"#1"}},
			wantCite: nil,
		},
		{
			name:     "U4 fires on no declared depends_on",
			filing:   Filing{ParentEpicRef: "#54"},
			wantCite: []string{"U4"},
		},
		{
			name:     "U4 does not fire when a depends_on edge is declared",
			filing:   Filing{ParentEpicRef: "#54", DependsOn: []string{"#2230"}},
			wantCite: nil,
		},
		{
			name:     "all three fire together in rule order",
			filing:   Filing{MissingLabelNamespaces: []string{"phase"}},
			dups:     mediumDup,
			wantCite: []string{"S2", "S4", "U4"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreFiling(tc.filing, tc.dups, Charter{Path: charterPath, RubricIDs: rubric, Resolved: true})
			gotIDs := citedIDs(got)
			if strings.Join(gotIDs, ",") != strings.Join(tc.wantCite, ",") {
				t.Fatalf("citations = %v, want %v", gotIDs, tc.wantCite)
			}
			if len(tc.wantCite) == 0 {
				if !got.Unscored || got.CharterGap == "" {
					t.Fatalf("no citation must yield Unscored with a gap, got %+v", got)
				}
				if got.Value != 0 {
					t.Fatalf("Value = %v, want 0 when nothing is cited", got.Value)
				}
				return
			}
			if got.Unscored {
				t.Fatalf("Unscored set despite citations %v", gotIDs)
			}
			if got.Value <= 0 {
				t.Fatalf("Value = %v, want a positive advisory weight", got.Value)
			}
			for _, c := range got.Citations {
				if c.Quote == "" {
					t.Errorf("%s cited with no charter quote", c.RubricID)
				}
				if c.Note == "" {
					t.Errorf("%s cited with no note saying what fired it", c.RubricID)
				}
			}
		})
	}
}

// TestScore_DropsCitationForIdAbsentFromCharter builds the bad state BY
// CONSTRUCTION — a charter fixture that simply omits the S4 row — rather
// than by calling the control. It is the counterfactual vehicle for the
// "the charter is authoritative" drop in ScoreFiling.
func TestScore_DropsCitationForIdAbsentFromCharter(t *testing.T) {
	withoutS4 := ParseRubricIDs("| **S2** | Superseded or duplicated by another item. |\n| **U4** | Blocks nothing. |\n")
	if withoutS4.Has("S4") {
		t.Fatal("fixture is wrong: it must NOT declare S4")
	}
	// A filing that fires S4 (no parent epic) and U4 (no depends_on).
	filing := Filing{MissingLabelNamespaces: []string{"phase"}}

	got := ScoreFiling(filing, nil, Charter{Path: charterPath, RubricIDs: withoutS4, Resolved: true})

	for _, c := range got.Citations {
		if c.RubricID == "S4" {
			t.Fatalf("cited S4, which the charter does not declare: %+v", c)
		}
	}
	if len(got.Citations) != 1 || got.Citations[0].RubricID != "U4" {
		t.Fatalf("want only the declared U4 citation, got %v", citedIDs(got))
	}
	if got.Value != weightU4 {
		t.Fatalf("Value = %v, want %v — a dropped citation must drop its weight too", got.Value, weightU4)
	}
}

// TestScore_UnscoredRecordsCharterGap pins the no-fabricated-citation gate:
// with a charter declaring none of the ids the rules cite, the result is an
// explicit gap rather than a synthesized citation.
func TestScore_UnscoredRecordsCharterGap(t *testing.T) {
	unrelated := ParseRubricIDs("| **V1** | Directly unblocks the current phase. |\n")
	filing := Filing{MissingLabelNamespaces: []string{"phase"}}

	got := ScoreFiling(filing, nil, Charter{Path: charterPath, RubricIDs: unrelated, Resolved: true})

	if len(got.Citations) != 0 {
		t.Fatalf("want no citation, got %v", citedIDs(got))
	}
	if !got.Unscored {
		t.Fatal("want Unscored=true — a gap is a finding, not a reason to invent a citation")
	}
	if !strings.Contains(got.CharterGap, "S4") || !strings.Contains(got.CharterGap, "U4") {
		t.Fatalf("CharterGap must name the rules that fired but could not be cited, got %q", got.CharterGap)
	}
}

// TestScoreFiling_EmptyRubricGapNamesTheCauseNotTheParser covers the three arms of the
// empty-rubric gap, is the pure-package half of the #2827 fix: an empty Rubric
// has three distinct causes and each must produce its OWN cause-naming gap
// string. Before this change all three produced one string blaming the parser,
// which is what sent an operator to inspect a healthy parser for a document
// that had never been fetched.
//
// The table asserts BOTH that each arm names its own cause AND that the three
// strings are pairwise DISTINCT — the distinctness half is what a future edit
// collapsing two arms back together would redden.
func TestScoreFiling_EmptyRubricGapNamesTheCauseNotTheParser(t *testing.T) {
	tests := []struct {
		name    string
		charter Charter
		// wantSubstr is the phrase that names THIS arm's cause.
		wantSubstr string
		// notSubstr must NOT appear: an unread charter must never be reported
		// as a parse failure, which is the exact confusion #2827 is about.
		notSubstr string
	}{
		{
			name:       "declared but never read names the read, not the parser",
			charter:    Charter{Path: charterPath},
			wantSubstr: charterPath + " was not read",
			notSubstr:  "could not be parsed",
		},
		{
			name:       "no charter declared at all names the declaration",
			charter:    Charter{},
			wantSubstr: "no charter is declared for this repository",
			notSubstr:  "parsed",
		},
		{
			name:       "read but carrying no rubric table names the parse",
			charter:    Charter{Path: charterPath, Resolved: true},
			wantSubstr: "no rubric lines could be parsed from " + charterPath,
			notSubstr:  "was not read",
		},
	}

	seen := map[string]string{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreFiling(Filing{}, nil, tc.charter)
			if !got.Unscored {
				t.Fatalf("want Unscored with an empty rubric, got %+v", got)
			}
			if !strings.Contains(got.CharterGap, tc.wantSubstr) {
				t.Fatalf("CharterGap = %q, want it to contain %q — the gap must name the cause", got.CharterGap, tc.wantSubstr)
			}
			if strings.Contains(got.CharterGap, tc.notSubstr) {
				t.Fatalf("CharterGap = %q must NOT contain %q — that names a different cause than the one that occurred", got.CharterGap, tc.notSubstr)
			}
			if prev, dup := seen[got.CharterGap]; dup {
				t.Fatalf("CharterGap %q is shared with %q; the three causes must be distinguishable from the gap alone", got.CharterGap, prev)
			}
			seen[got.CharterGap] = tc.name
		})
	}
	if len(seen) != len(tests) {
		t.Fatalf("got %d distinct gap strings across %d arms: %v", len(seen), len(tests), seen)
	}
}

func TestScoreFiling_UnscoredWhenNoRuleFires(t *testing.T) {
	rubric := ParseRubricIDs(rubricFixture)
	complete := Filing{ParentEpicRef: "#54", DependsOn: []string{"#2230"}}

	got := ScoreFiling(complete, nil, Charter{Path: charterPath, RubricIDs: rubric, Resolved: true})
	if !got.Unscored {
		t.Fatalf("want Unscored for a structurally complete filing, got %+v", got)
	}
	if !strings.Contains(got.CharterGap, "no decidable structural rubric line") {
		t.Fatalf("CharterGap = %q, want the no-rule-fired gap", got.CharterGap)
	}
}
