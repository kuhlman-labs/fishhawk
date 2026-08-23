package intakegroom

import (
	"testing"
)

func TestTokenize_StripsConventionsPrefixStopwordsAndPunctuation(t *testing.T) {
	got := tokenize("[E54.7] Add the intake hook, for filing!")
	want := []string{"intake", "hook", "filing"}
	if len(got) != len(want) {
		t.Fatalf("tokenize = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokenize = %v, want %v", got, want)
		}
	}
	if len(tokenize("[ADR-065] the a of and")) != 0 {
		t.Fatalf("a stopword-only title must tokenize to nothing, got %v", tokenize("[ADR-065] the a of and"))
	}
}

func TestDuplicates_ConfidenceBands(t *testing.T) {
	filing := Filing{Title: "intake hook duplicate epic score render"}
	tests := []struct {
		name         string
		candidate    string
		wantReported bool
		wantBand     Confidence
	}{
		// 6/6 shared, union 6 -> 1.00
		{"identical tokens is high", "intake hook duplicate epic score render", true, ConfidenceHigh},
		// 4 shared, union 8 -> 0.500 -> medium
		{"four of eight is medium", "intake hook duplicate epic charter parse", true, ConfidenceMedium},
		// 3 shared, union 9 -> 0.333 -> low
		{"three of nine is low", "intake hook duplicate charter parse rubric", true, ConfidenceLow},
		// 1 shared, union 11 -> 0.09 -> below the floor
		{"one shared token is not a candidate", "render helm chart ingress secret migration hook job rollout probe", false, ""},
		{"no shared tokens is not a candidate", "helm chart ingress secrets", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Duplicates(filing, []Candidate{{Number: 7, Title: tc.candidate}})
			if !tc.wantReported {
				if len(got) != 0 {
					t.Fatalf("want no candidate, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want one candidate, got %+v", got)
			}
			if got[0].Confidence != tc.wantBand {
				t.Fatalf("confidence = %q (score %.3f), want %q", got[0].Confidence, got[0].Score, tc.wantBand)
			}
			if got[0].Basis == "" {
				t.Fatal("want a rendered shared-token basis — it is what makes a wrong candidate cheap to dismiss")
			}
		})
	}
}

// TestDuplicates_BelowFloorIsNotACandidate is the counterfactual vehicle for
// the ThresholdLow floor: delete the band gate in Duplicates and a
// one-token-overlap pair is reported.
func TestDuplicates_BelowFloorIsNotACandidate(t *testing.T) {
	filing := Filing{Title: "intake hook duplicate epic score render marker"}
	candidate := Candidate{Number: 9, Title: "marker helm chart ingress secret migration probe rollout"}

	if got := Duplicates(filing, []Candidate{candidate}); len(got) != 0 {
		t.Fatalf("a sub-floor pair must not be a candidate, got %+v (score %.3f)", got, got[0].Score)
	}
}

func TestDuplicates_DeterministicUnderTiedScores(t *testing.T) {
	filing := Filing{Title: "intake hook duplicate detection"}
	// Three candidates with byte-identical titles: every score ties, so only
	// the number tiebreak can order them.
	candidates := []Candidate{
		{Number: 300, Title: "intake hook duplicate detection"},
		{Number: 100, Title: "intake hook duplicate detection"},
		{Number: 200, Title: "intake hook duplicate detection"},
	}
	for i := 0; i < 20; i++ {
		got := Duplicates(filing, candidates)
		if len(got) != 3 {
			t.Fatalf("want 3 candidates, got %d", len(got))
		}
		if got[0].Number != 100 || got[1].Number != 200 || got[2].Number != 300 {
			t.Fatalf("tied scores must order by number ASC, got %d,%d,%d", got[0].Number, got[1].Number, got[2].Number)
		}
	}
}

func TestDuplicates_CapsAtMaxDuplicates(t *testing.T) {
	filing := Filing{Title: "intake hook duplicate detection"}
	var candidates []Candidate
	for n := 1; n <= MaxDuplicates+3; n++ {
		candidates = append(candidates, Candidate{Number: n, Title: "intake hook duplicate detection"})
	}
	got := Duplicates(filing, candidates)
	if len(got) != MaxDuplicates {
		t.Fatalf("len = %d, want the MaxDuplicates cap of %d", len(got), MaxDuplicates)
	}
}

func TestDuplicates_SelfMatchIsNotADuplicate(t *testing.T) {
	filing := Filing{Title: "intake hook duplicate detection", Body: "the body as rendered"}
	echoed := Candidate{Number: 42, Title: filing.Title, Body: filing.Body}

	if got := Duplicates(filing, []Candidate{echoed}); len(got) != 0 {
		t.Fatalf("the filing echoed back must not be its own duplicate, got %+v", got)
	}

	// The guard is deliberately narrow. A same-title item with a DIFFERENT
	// body is still the strongest duplicate there is, and two BODYLESS items
	// sharing a title are a genuine pair — neither may be swallowed.
	sameTitle := Candidate{Number: 43, Title: filing.Title, Body: "a genuinely different body"}
	got := Duplicates(filing, []Candidate{sameTitle})
	if len(got) != 1 || got[0].Confidence != ConfidenceHigh {
		t.Fatalf("want a high-confidence duplicate for a same-title different-body item, got %+v", got)
	}

	bodyless := Filing{Title: "intake hook duplicate detection"}
	got = Duplicates(bodyless, []Candidate{{Number: 44, Title: bodyless.Title}})
	if len(got) != 1 || got[0].Confidence != ConfidenceHigh {
		t.Fatalf("want a high-confidence duplicate for two bodyless same-title items, got %+v", got)
	}
}

func TestDuplicates_UnscoreableTitlesYieldNothingNotADivideByZero(t *testing.T) {
	tests := []struct {
		name   string
		filing string
		cand   string
	}{
		{"empty filing title", "", "intake hook duplicate"},
		{"whitespace filing title", "   \t ", "intake hook duplicate"},
		{"empty candidate title", "intake hook duplicate", ""},
		{"stopword-only filing title", "[E1] the a of and to", "intake hook duplicate"},
		{"stopword-only candidate title", "intake hook duplicate", "the a of and to"},
		{"both empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Duplicates(Filing{Title: tc.filing}, []Candidate{{Number: 1, Title: tc.cand}})
			if len(got) != 0 {
				t.Fatalf("want no candidate for an unscoreable pair, got %+v", got)
			}
		})
	}
}

func TestDuplicates_SameTypeLabelAddsBonus(t *testing.T) {
	filing := Filing{Title: "intake hook duplicate epic charter", Type: "feature", Labels: []string{"type:feature"}}
	title := "intake hook duplicate charter rubric parse"

	without := Duplicates(filing, []Candidate{{Number: 1, Title: title}})
	with := Duplicates(filing, []Candidate{{Number: 1, Title: title, Labels: []string{"type:feature"}}})

	if len(without) != 1 || len(with) != 1 {
		t.Fatalf("want one candidate each, got %d and %d", len(without), len(with))
	}
	if delta := with[0].Score - without[0].Score; delta < typeLabelBonus-1e-9 || delta > typeLabelBonus+1e-9 {
		t.Fatalf("same-type bonus = %.4f, want %.4f", delta, typeLabelBonus)
	}
}

func TestDuplicates_ScoreNeverExceedsOne(t *testing.T) {
	filing := Filing{Title: "intake hook duplicate", Type: "feature", Labels: []string{"type:feature"}}
	got := Duplicates(filing, []Candidate{{Number: 1, Title: "intake hook duplicate", Labels: []string{"type:feature"}}})
	if len(got) != 1 {
		t.Fatalf("want one candidate, got %+v", got)
	}
	if got[0].Score > 1.0 {
		t.Fatalf("score = %.4f, must be capped at 1.0", got[0].Score)
	}
}

func TestDuplicates_ClosedCandidateIsStillReported(t *testing.T) {
	filing := Filing{Title: "intake hook duplicate detection"}
	got := Duplicates(filing, []Candidate{{Number: 5, Title: "intake hook duplicate detection", Closed: true}})
	if len(got) != 1 || !got[0].Closed {
		t.Fatalf("a closed duplicate is often the resolution and must be reported, got %+v", got)
	}
}

func TestJaccard_EmptyUnionIsZeroNotNaN(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
	}{
		{"both empty", nil, nil},
		{"left empty", nil, []string{"intake"}},
		{"right empty", []string{"intake"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, shared := jaccard(tc.a, tc.b)
			if got != 0 {
				// NaN != 0 is true, which is exactly how this catches a
				// 0/0 division: every threshold comparison against NaN is
				// false, so a NaN score fails silently rather than loudly.
				t.Fatalf("jaccard = %v, want exactly 0", got)
			}
			if len(shared) != 0 {
				t.Fatalf("shared = %v, want none", shared)
			}
		})
	}
}
