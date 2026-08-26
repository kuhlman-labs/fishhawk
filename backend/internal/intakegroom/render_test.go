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
		// A score is Unscored with a stated gap or it cites — ScoreFiling
		// never emits a scored-but-uncited verdict, and ParseBody now
		// enforces that invariant on read-back, so the fixture states it.
		Score:        Score{Unscored: true, CharterGap: "no decidable structural rubric line fires for this filing"},
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

// validPayload is the minimal marker payload ParseBody accepts: every field
// marker() always emits, no field it does not, and a Signals that satisfies
// every invariant this package's producers establish.
//
// Every malformed fixture below is derived from THIS string by a single
// targeted replacement rather than written independently, so a rejection can
// only be attributed to the one edit — a fixture that differed from the
// accepted payload in some second, incidental way would be refused with or
// without the control it claims to exercise.
const validPayload = `{"score":{"value":0,"unscored":true,"charter_gap":"none"},` +
	`"degraded":false,"scanned_items":0,"window_truncated":false,"duration_ms":0}`

// validDupPayload is validPayload carrying one well-formed duplicate
// candidate — the base for the per-candidate invariant fixtures.
var validDupPayload = mutate(validPayload, `"degraded":false`,
	`"duplicates":[{"number":7,"title":"a real duplicate","score":0.8,"confidence":"high","basis":"real","closed":false}],"degraded":false`)

// mutate returns base with the single occurrence of old replaced by new,
// failing loudly at init time if old is not present (a fixture that silently
// stopped mutating anything would be a vacuous test case).
func mutate(base, old, new string) string {
	if !strings.Contains(base, old) {
		panic("fixture base does not contain " + old)
	}
	return strings.Replace(base, old, new, 1)
}

// markerBody wraps a raw payload in a marker inside an ordinary body.
func markerBody(payload string) string {
	return "body\n" + MarkerPrefix + payload + MarkerSuffix
}

// TestParseBody_AcceptsTheWellFormedFixtureBases proves the two bases the
// rejection fixtures are derived from are themselves ACCEPTED. Without this,
// every rejection below could be passing for a reason unrelated to its edit.
func TestParseBody_AcceptsTheWellFormedFixtureBases(t *testing.T) {
	for name, payload := range map[string]string{
		"minimal":            validPayload,
		"with a duplicate":   validDupPayload,
		"with a trailing NL": validPayload + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := ParseBody(markerBody(payload)); !ok {
				t.Fatalf("the fixture base must parse, got ok=false:\n%s", payload)
			}
		})
	}
}

func TestParseBody_RejectsMalformedMarkers(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no marker at all", "just a body\n"},
		{"unterminated marker", "body\n" + MarkerPrefix + `{"scanned_items":1}`},
		{"payload is not json", markerBody("{not json}")},
		{"payload is empty", "body\n" + MarkerPrefix + MarkerSuffix},
		{"payload is a bare string", markerBody(`"nope"`)},
		{"payload is truncated json", markerBody(`{"scanned_items":`)},
		{"payload carries an unknown field", markerBody(
			mutate(validPayload, `"degraded":false`, `"degraded":false,"invented":true`))},

		// EXACTLY ONE VALUE. A json.Decoder stops at the end of the first
		// value, so both of these decode a complete, valid Signals first and
		// are only rejected by the EOF check.
		{"a second JSON value follows the payload", markerBody(validPayload + " " + validPayload)},
		{"a second JSON value of another type follows", markerBody(validPayload + " 42")},
		{"trailing non-whitespace junk follows", markerBody(validPayload + " junk")},

		// FIELD PRESENCE. Each of these decodes into a Signals without
		// error, so only the payload's raw key set can reject it. The two
		// omit-a-scalar cases are attained by the presence check ALONE; the
		// empty object and the omitted score additionally violate a Signals
		// invariant, so they are refused twice over.
		{"payload is an empty object", markerBody("{}")},
		{"payload omits duration_ms", markerBody(mutate(validPayload, `,"duration_ms":0`, ""))},
		{"payload omits score", markerBody(mutate(validPayload,
			`"score":{"value":0,"unscored":true,"charter_gap":"none"},`, ""))},
		{"payload omits scanned_items", markerBody(mutate(validPayload, `"scanned_items":0,`, ""))},

		// SIGNALS INVARIANTS. Each decodes into a well-typed Signals that no
		// producer path in this package can emit.
		{"negative scanned_items", markerBody(mutate(validPayload, `"scanned_items":0`, `"scanned_items":-1`))},
		{"negative duration_ms", markerBody(mutate(validPayload, `"duration_ms":0`, `"duration_ms":-1`))},
		{"degraded with no reason", markerBody(mutate(validPayload, `"degraded":false`, `"degraded":true`))},
		{"a reason without the degraded flag", markerBody(mutate(validPayload,
			`"degraded":false`, `"degraded":false,"degrade_reason":"reader_error"`))},
		{"a degrade reason outside the closed set", markerBody(mutate(validPayload,
			`"degraded":false`, `"degraded":true,"degrade_reason":"invented_reason"`))},
		{"unscored while citing a rubric line", markerBody(mutate(validPayload,
			`"unscored":true`, `"unscored":true,"citations":[{"rubric_id":"S2","quote":"q","note":"n"}]`))},
		{"scored with no citation at all", markerBody(mutate(validPayload,
			`"unscored":true,"charter_gap":"none"`, `"unscored":false`))},
		{"unscored with no stated gap", markerBody(mutate(validPayload, `"charter_gap":"none"`, `"charter_gap":"  "`))},
		{"a citation with no rubric id", markerBody(mutate(validPayload,
			`"unscored":true,"charter_gap":"none"`, `"unscored":false,"citations":[{"rubric_id":"","quote":"q","note":"n"}]`))},
		{"a citation quoting nothing", markerBody(mutate(validPayload,
			`"unscored":true,"charter_gap":"none"`, `"unscored":false,"citations":[{"rubric_id":"S2","quote":"","note":"n"}]`))},
		{"a negative score value", markerBody(mutate(validPayload, `"value":0`, `"value":-3`))},

		// PER-CANDIDATE INVARIANTS, derived from validDupPayload.
		{"a duplicate that is an empty object", markerBody(mutate(validDupPayload,
			`{"number":7,"title":"a real duplicate","score":0.8,"confidence":"high","basis":"real","closed":false}`, "{}"))},
		{"a duplicate with no tracker number", markerBody(mutate(validDupPayload, `"number":7`, `"number":0`))},
		{"a duplicate with an untitled item", markerBody(mutate(validDupPayload, `"title":"a real duplicate"`, `"title":""`))},
		{"a duplicate scoring above 1", markerBody(mutate(validDupPayload, `"score":0.8`, `"score":1.5`))},
		{"a duplicate with an unknown confidence band", markerBody(mutate(validDupPayload, `"confidence":"high"`, `"confidence":"certain"`))},
		{"more duplicates than MaxDuplicates", markerBody(mutate(validDupPayload, `"duplicates":[`,
			`"duplicates":[{"number":1,"title":"a","score":0.5,"confidence":"medium","basis":"a"},`+
				`{"number":2,"title":"b","score":0.5,"confidence":"medium","basis":"b"},`+
				`{"number":3,"title":"c","score":0.5,"confidence":"medium","basis":"c"},`))},
		{"an epic suggestion with no tracker number", markerBody(mutate(validPayload, `"degraded":false`,
			`"epic_suggestion":{"number":0,"title":"an epic","score":0.4,"confidence":"low","basis":"e"},"degraded":false`))},
		{"an epic suggestion with an unknown band", markerBody(mutate(validPayload, `"degraded":false`,
			`"epic_suggestion":{"number":54,"title":"an epic","score":0.4,"confidence":"probably","basis":"e"},"degraded":false`))},
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
		s.Score = ScoreFiling(Filing{}, nil, Charter{}) // Unscored, no citations.
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
