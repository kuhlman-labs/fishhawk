package intakegroom

import (
	"encoding/json"
	"strings"
	"testing"
)

func fullSignals() Signals {
	return Signals{
		Duplicates: []DuplicateCandidate{
			{Number: 2239, URL: "https://example.test/2239", Title: "[E54.7] Intake hook", Score: 0.72, Confidence: ConfidenceHigh, Basis: "hook, intake", Closed: false},
			{Number: 2100, URL: "https://example.test/2100", Title: "Older intake work", Score: 0.46, Confidence: ConfidenceMedium, Basis: "intake", Closed: true},
		},
		EpicSuggestion: &EpicSuggestion{Number: 54, URL: "https://example.test/54", Title: "[E54] Backlog grooming", Score: 0.31, Confidence: ConfidenceLow, Basis: "grooming"},
		Score: Score{
			Value:     3.5,
			Citations: []Citation{{RubricID: "S2", Quote: "Superseded or duplicated by another item.", Note: "a high-confidence duplicate candidate was found (#2239)"}},
		},
		ScannedItems:    300,
		WindowTruncated: true,
		DurationMS:      412,
	}
}

func TestRenderBody_RoundTripsThroughParseBody(t *testing.T) {
	original := "Original body text.\n"
	rendered := RenderBody(original, fullSignals())

	if !strings.Contains(rendered, original) {
		t.Fatal("the original body must be preserved")
	}
	if !strings.Contains(rendered, SectionHeading) {
		t.Fatalf("missing the advisory heading:\n%s", rendered)
	}
	if !strings.Contains(rendered, "#2239") || !strings.Contains(rendered, "#54") || !strings.Contains(rendered, "S2") {
		t.Fatalf("advisory section is missing a rendered signal:\n%s", rendered)
	}

	got, ok := ParseBody(rendered)
	if !ok {
		t.Fatalf("ParseBody failed on our own rendering:\n%s", rendered)
	}
	wantJSON, _ := json.Marshal(fullSignals())
	gotJSON, _ := json.Marshal(got)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("round trip lost data:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

// TestRenderBody_RoundTripsATitleContainingTheCommentTerminator is the
// counterfactual vehicle for escapeAngle: without it the "-->" inside the
// title closes the HTML comment early and the marker no longer decodes.
func TestRenderBody_RoundTripsATitleContainingTheCommentTerminator(t *testing.T) {
	s := Signals{
		Duplicates: []DuplicateCandidate{{
			Number:     7,
			Title:      "Break out of the marker --> and keep going <!-- again",
			Score:      0.9,
			Confidence: ConfidenceHigh,
			Basis:      "marker",
		}},
		ScannedItems: 1,
	}

	rendered := RenderBody("body", s)
	if strings.Count(rendered, MarkerPrefix) != 1 {
		t.Fatalf("want exactly one marker, got %d:\n%s", strings.Count(rendered, MarkerPrefix), rendered)
	}

	got, ok := ParseBody(rendered)
	if !ok {
		t.Fatalf("ParseBody failed — the title's terminator escaped the marker:\n%s", rendered)
	}
	if len(got.Duplicates) != 1 || got.Duplicates[0].Title != s.Duplicates[0].Title {
		t.Fatalf("title did not survive the round trip: %+v", got.Duplicates)
	}
}

func TestParseBody_RejectsMalformedMarkers(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no marker at all", "just a body\n"},
		{"unterminated marker", "body\n" + MarkerPrefix + `{"scanned_items":1}`},
		{"payload is not json", "body\n" + MarkerPrefix + "{not json}" + MarkerSuffix},
		{"payload is empty", "body\n" + MarkerPrefix + MarkerSuffix},
		{"payload is a bare string", "body\n" + MarkerPrefix + `"nope"` + MarkerSuffix},
		{"payload carries an unknown field", "body\n" + MarkerPrefix + `{"scanned_items":1,"invented":true}` + MarkerSuffix},
		{"payload is truncated json", "body\n" + MarkerPrefix + `{"scanned_items":` + MarkerSuffix},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseBody(tc.body)
			if ok {
				t.Fatalf("want ok=false, got %+v", got)
			}
			if got.HasFindings() || got.ScannedItems != 0 || got.Degraded {
				t.Fatalf("a rejected marker must yield the zero Signals, got %+v", got)
			}
		})
	}
}

func TestParseBody_ReadsTheLastMarkerWhenTheBodyQuotesAnEarlierOne(t *testing.T) {
	quoted := "Someone pasted " + MarkerPrefix + `{"scanned_items":99}` + MarkerSuffix + " into the body.\n"
	rendered := RenderBody(quoted, fullSignals())

	got, ok := ParseBody(rendered)
	if !ok {
		t.Fatal("ParseBody failed")
	}
	if got.ScannedItems != 300 {
		t.Fatalf("ScannedItems = %d, want the appended marker's 300, not the quoted 99", got.ScannedItems)
	}
}

// TestRenderBody_DegradedWithNoFindingsIsAByteIdenticalNoOp pins the promise
// that a degraded hook is invisible on the filing path.
func TestRenderBody_DegradedWithNoFindingsIsAByteIdenticalNoOp(t *testing.T) {
	body := "## Summary\n\nThe original body, unchanged.\n"
	for _, reason := range DegradeReasons() {
		s := Degrade(reason)
		s.Score = ScoreFiling(Filing{}, nil, Rubric{}) // Unscored, no citations.
		if got := RenderBody(body, s); got != body {
			t.Fatalf("reason %q: body changed under a degraded hook:\n%s", reason, got)
		}
		if _, ok := ParseBody(RenderBody(body, s)); ok {
			t.Fatalf("reason %q: a no-op render must leave no marker", reason)
		}
	}
}

func TestRenderBody_DegradedButWithFindingsStillRenders(t *testing.T) {
	s := Degrade(DegradeReasonCharterRubricUnparsed)
	s.Duplicates = []DuplicateCandidate{{Number: 7, Title: "a real duplicate", Score: 0.8, Confidence: ConfidenceHigh, Basis: "real"}}
	s.Score = Score{Unscored: true, CharterGap: "no rubric lines could be parsed from the charter"}

	got := RenderBody("body", s)
	if !strings.Contains(got, "#7") {
		t.Fatalf("a finding derived before the degradation must still be rendered:\n%s", got)
	}
	if !strings.Contains(got, string(DegradeReasonCharterRubricUnparsed)) {
		t.Fatalf("the degrade reason must be stated in the section:\n%s", got)
	}
	if _, ok := ParseBody(got); !ok {
		t.Fatal("the marker must still round trip")
	}
}

func TestRenderBody_ReportsAnEmptyResultAndATruncatedWindow(t *testing.T) {
	s := Signals{
		Score:           Score{Unscored: true, CharterGap: "no decidable structural rubric line fires for this filing"},
		ScannedItems:    300,
		WindowTruncated: true,
	}
	// Not degraded, no findings: a healthy scan that simply found nothing
	// still renders, so a reader can tell "we looked" from "we did not".
	got := RenderBody("body", s)
	if !strings.Contains(got, "none found") {
		t.Fatalf("want an explicit empty-duplicates line:\n%s", got)
	}
	if !strings.Contains(got, "Parent epic suggestion") || !strings.Contains(got, "- none\n") {
		t.Fatalf("want an explicit no-epic-suggestion line:\n%s", got)
	}
	if !strings.Contains(got, "unscored:") || !strings.Contains(got, "no decidable structural rubric line") {
		t.Fatalf("want the charter-gap note rendered:\n%s", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("want the truncated window stated:\n%s", got)
	}
	if !strings.Contains(got, "Scanned 300") {
		t.Fatalf("want the scanned count stated:\n%s", got)
	}
}

func TestRenderBody_EmptyOriginalBodyGetsNoLeadingBlankLines(t *testing.T) {
	got := RenderBody("", fullSignals())
	if !strings.HasPrefix(got, SectionHeading) {
		t.Fatalf("want the section to open the body, got:\n%q", got[:min(80, len(got))])
	}
}

func TestRenderBody_StatesTheNothingDestructivePosture(t *testing.T) {
	got := RenderBody("body", fullSignals())
	if !strings.Contains(got, "nothing was closed, relabelled or transitioned") {
		t.Fatalf("the body must state the posture where its reader sees it:\n%s", got)
	}
}
