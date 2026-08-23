package intakegroom

import "testing"

// TestSuggestEpic_ChildTitleIsNeverSuggestedAsAnEpic is the counterfactual
// vehicle for the childless-[E<n>] discrimination: a sibling child issue
// shares far more vocabulary with the filing than the real epic does, so if
// the child-suffix discrimination is removed the child wins the suggestion.
func TestSuggestEpic_ChildTitleIsNeverSuggestedAsAnEpic(t *testing.T) {
	f := Filing{Title: "[E54.9] Backlog grooming intake hook duplicate detection"}
	candidates := []Candidate{
		{Number: 2239, Title: "[E54.7] Backlog grooming intake hook duplicate detection"},
		{Number: 54, Title: "[E54] Backlog grooming"},
	}

	got := SuggestEpic(f, candidates)
	if got == nil {
		t.Fatal("want the epic suggested")
	}
	if got.Number != 54 {
		t.Fatalf("suggested #%d (%q) — a [E54.7] child is not a parent epic", got.Number, got.Title)
	}
}

func TestSuggestEpic_TypeEpicLabelQualifies(t *testing.T) {
	f := Filing{Title: "backlog grooming intake hook"}
	// No conventions prefix at all — only the label makes this an epic.
	candidates := []Candidate{{Number: 7, Title: "Backlog grooming", Labels: []string{"type:epic"}}}

	got := SuggestEpic(f, candidates)
	if got == nil || got.Number != 7 {
		t.Fatalf("want #7 suggested via its type:epic label, got %+v", got)
	}
}

func TestSuggestEpic_BelowThresholdReturnsNil(t *testing.T) {
	f := Filing{Title: "intake hook duplicate detection scoring render marker"}
	candidates := []Candidate{{Number: 40, Title: "[E40] Helm chart ingress secrets rollout"}}

	if got := SuggestEpic(f, candidates); got != nil {
		t.Fatalf("want nil below EpicThreshold, got %+v (score %.3f)", got, got.Score)
	}
}

func TestSuggestEpic_TieBreaksOnLowerNumber(t *testing.T) {
	f := Filing{Title: "backlog grooming intake"}
	candidates := []Candidate{
		{Number: 90, Title: "[E90] Backlog grooming intake"},
		{Number: 12, Title: "[E12] Backlog grooming intake"},
		{Number: 55, Title: "[E55] Backlog grooming intake"},
	}
	for i := 0; i < 20; i++ {
		got := SuggestEpic(f, candidates)
		if got == nil || got.Number != 12 {
			t.Fatalf("tied epics must break on the lower number, got %+v", got)
		}
	}
}

func TestSuggestEpic_SharedAreaLabelAddsBonus(t *testing.T) {
	f := Filing{Title: "backlog grooming intake hook duplicate detection", Labels: []string{"area:backend"}}
	title := "[E54] Backlog grooming decision-ready tracker"

	without := SuggestEpic(f, []Candidate{{Number: 54, Title: title}})
	with := SuggestEpic(f, []Candidate{{Number: 54, Title: title, Labels: []string{"area:backend"}}})
	if without == nil || with == nil {
		t.Fatalf("want a suggestion in both cases, got %+v and %+v", without, with)
	}
	if delta := with.Score - without.Score; delta < areaLabelBonus-1e-9 || delta > areaLabelBonus+1e-9 {
		t.Fatalf("area bonus = %.4f, want %.4f", delta, areaLabelBonus)
	}
}

func TestSuggestEpic_UnscoreableFilingTitleReturnsNil(t *testing.T) {
	candidates := []Candidate{{Number: 54, Title: "[E54] Backlog grooming"}}
	for _, title := range []string{"", "   ", "[E1] the a of and"} {
		if got := SuggestEpic(Filing{Title: title}, candidates); got != nil {
			t.Fatalf("title %q: want nil, got %+v", title, got)
		}
	}
}

func TestSuggestEpic_NoEpicsInWindowReturnsNil(t *testing.T) {
	f := Filing{Title: "backlog grooming intake hook"}
	candidates := []Candidate{{Number: 2239, Title: "[E54.7] Backlog grooming intake hook"}}

	if got := SuggestEpic(f, candidates); got != nil {
		t.Fatalf("want nil when the window holds no epic, got %+v", got)
	}
}

func TestSuggestEpic_LowOverlapIsReportedAtTheLowBand(t *testing.T) {
	// Above EpicThreshold (0.15) but below the duplicate floor (0.30): the
	// suggestion is real and must be reported, banded low rather than dropped.
	f := Filing{Title: "backlog grooming intake hook duplicate detection render"}
	got := SuggestEpic(f, []Candidate{{Number: 54, Title: "[E54] Backlog grooming"}})
	if got == nil {
		t.Fatal("want a suggestion above EpicThreshold")
	}
	if got.Score >= ThresholdLow {
		t.Fatalf("fixture drifted: score %.3f is no longer below the duplicate floor", got.Score)
	}
	if got.Confidence != ConfidenceLow {
		t.Fatalf("confidence = %q, want %q", got.Confidence, ConfidenceLow)
	}
}

func TestSuggestEpic_ScoreNeverExceedsOne(t *testing.T) {
	f := Filing{Title: "backlog grooming", Labels: []string{"area:backend"}}
	got := SuggestEpic(f, []Candidate{{Number: 54, Title: "[E54] Backlog grooming", Labels: []string{"area:backend"}}})
	if got == nil {
		t.Fatal("want a suggestion")
	}
	if got.Score > 1.0 {
		t.Fatalf("score = %.4f, must be capped at 1.0", got.Score)
	}
}
