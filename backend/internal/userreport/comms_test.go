package userreport

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

var (
	hashA = strings.Repeat("a", 64)
	hashB = strings.Repeat("b", 64)
	hashC = strings.Repeat("c", 64)
)

// marker spells a comms marker around a raw payload, so a test can build a
// MALFORMED marker by construction rather than through DraftMarker.
func marker(payload string) string {
	return CommsMarkerPrefix + payload + commsMarkerSuffix
}

// TestDraftMarker_ExactBytes pins the rendered form: ONE line, prefix and
// suffix, entries sorted by id and deduped on (id, content_hash).
func TestDraftMarker_ExactBytes(t *testing.T) {
	got := DraftMarker([]MarkedReport{
		{ID: "UR-issue-9", ContentHash: hashA},
		{ID: "UR-comment-3-4", ContentHash: hashB},
		{ID: "UR-issue-9", ContentHash: hashA}, // duplicate
	})
	want := `<!-- fishhawk-comms:v1 {"reports":[{"id":"UR-comment-3-4","content_hash":"` + hashB +
		`"},{"id":"UR-issue-9","content_hash":"` + hashA + `"}]} -->`
	if got != want {
		t.Errorf("DraftMarker =\n %q\nwant\n %q", got, want)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("DraftMarker spans lines: %q", got)
	}
}

// TestDraftMarker_RoundTrip is the producer/parser agreement: parsing a
// rendered marker returns exactly the canonical (valid, deduped, sorted) input.
func TestDraftMarker_RoundTrip(t *testing.T) {
	cases := map[string]struct {
		in   []MarkedReport
		want []MarkedReport
	}{
		"single issue": {
			in:   []MarkedReport{{ID: "UR-issue-1", ContentHash: hashA}},
			want: []MarkedReport{{ID: "UR-issue-1", ContentHash: hashA}},
		},
		"same id, two hashes kept and sorted by hash": {
			in:   []MarkedReport{{ID: "UR-issue-2", ContentHash: hashC}, {ID: "UR-issue-2", ContentHash: hashA}},
			want: []MarkedReport{{ID: "UR-issue-2", ContentHash: hashA}, {ID: "UR-issue-2", ContentHash: hashC}},
		},
		"mixed with invalid and duplicate": {
			in: []MarkedReport{
				{ID: "UR-issue-10", ContentHash: hashB},
				{ID: "UR-issue-01", ContentHash: hashB}, // invalid id
				{ID: "UR-comment-5-6", ContentHash: hashA},
				{ID: "UR-issue-10", ContentHash: hashB},
			},
			want: []MarkedReport{{ID: "UR-comment-5-6", ContentHash: hashA}, {ID: "UR-issue-10", ContentHash: hashB}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, malformed := ParseDraftMarkers(DraftMarker(tc.in))
			if !reflect.DeepEqual(got, tc.want) || malformed != 0 {
				t.Errorf("ParseDraftMarkers(DraftMarker(x)) = %+v, malformed %d; want %+v, malformed 0", got, malformed, tc.want)
			}
		})
	}
}

// TestDraftMarker_NothingValidRendersEmpty: DraftMarker renders "" for an
// empty input and for one whose every entry is invalid — including ids
// carrying "-->" or a line break, which must never reach a rendered marker.
// Mechanism: without the producer-side filter each of these renders a
// non-empty marker.
func TestDraftMarker_NothingValidRendersEmpty(t *testing.T) {
	cases := map[string][]MarkedReport{
		"nil":                {},
		"comment terminator": {{ID: "UR-issue-1 -->", ContentHash: hashA}},
		"line break":         {{ID: "UR-issue-1\n", ContentHash: hashA}},
		"upper-case hash":    {{ID: "UR-issue-1", ContentHash: strings.Repeat("A", 64)}},
		"unknown kind":       {{ID: "UR-unknown-1-2", ContentHash: hashA}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got := DraftMarker(in); got != "" {
				t.Errorf("DraftMarker(%+v) = %q, want \"\"", in, got)
			}
		})
	}
}

// TestParseDraftMarkers_ValidMarkers: a marker inside surrounding text is
// found, two markers in one body yield their union, and a well-formed empty
// list is NOT malformed.
func TestParseDraftMarkers_ValidMarkers(t *testing.T) {
	one := DraftMarker([]MarkedReport{{ID: "UR-issue-3", ContentHash: hashA}})
	two := DraftMarker([]MarkedReport{{ID: "UR-comment-3-11", ContentHash: hashB}, {ID: "UR-issue-3", ContentHash: hashA}})

	got, malformed := ParseDraftMarkers("Thanks — filed.\n\n" + one + "\n\nmore prose")
	if want := []MarkedReport{{ID: "UR-issue-3", ContentHash: hashA}}; !reflect.DeepEqual(got, want) || malformed != 0 {
		t.Errorf("surrounding text: got %+v malformed %d, want %+v malformed 0", got, malformed, want)
	}

	got, malformed = ParseDraftMarkers(one + "\n" + two)
	want := []MarkedReport{{ID: "UR-comment-3-11", ContentHash: hashB}, {ID: "UR-issue-3", ContentHash: hashA}}
	if !reflect.DeepEqual(got, want) || malformed != 0 {
		t.Errorf("two markers: got %+v malformed %d, want the union %+v malformed 0", got, malformed, want)
	}

	got, malformed = ParseDraftMarkers(marker(`{"reports":[]}`))
	if got != nil || malformed != 0 {
		t.Errorf("empty reports list: got %+v malformed %d, want nil malformed 0 (well-formed, no entries)", got, malformed)
	}

	got, malformed = ParseDraftMarkers("no marker here")
	if got != nil || malformed != 0 {
		t.Errorf("no marker: got %+v malformed %d, want nil malformed 0", got, malformed)
	}
}

// TestParseDraftMarkers_MalformedMarkerDropped pins one row per whole-marker
// drop rule. Each row places the bad marker beside a VALID marker in the same
// body, so the expected result is exactly the valid marker's entry: a bad
// marker whose rule is deleted leaks UR-issue-8 into the result. Every row
// also asserts the malformed count is 1 — the only observable for the
// missing-reports-key and null-reports rows, whose payload carries no entry.
func TestParseDraftMarkers_MalformedMarkerDropped(t *testing.T) {
	good := marker(`{"reports":[{"id":"UR-issue-7","content_hash":"` + hashA + `"}]}`)
	entry8 := `{"id":"UR-issue-8","content_hash":"` + hashB + `"}`
	want := []MarkedReport{{ID: "UR-issue-7", ContentHash: hashA}}

	cases := map[string]string{
		// After the good marker, so no later " -->" can close it.
		"unterminated":            good + "\n" + CommsMarkerPrefix + `{"reports":[` + entry8 + `]}`,
		"line break inside":       marker(`{"reports":[`+"\n"+entry8+`]}`) + "\n" + good,
		"malformed JSON":          marker(`{"reports":[`+entry8+`,`) + "\n" + good,
		"non-object payload":      marker(`[`+entry8+`]`) + "\n" + good,
		"unknown top-level field": marker(`{"reports":[`+entry8+`],"extra":1}`) + "\n" + good,
		"unknown nested field":    marker(`{"reports":[{"id":"UR-issue-8","content_hash":"`+hashB+`","note":"x"}]}`) + "\n" + good,
		"missing reports key":     marker(`{}`) + "\n" + good,
		"null reports":            marker(`{"reports":null}`) + "\n" + good,
		"trailing data":           marker(`{"reports":[`+entry8+`]} junk`) + "\n" + good,
		"two JSON values":         marker(`{"reports":[`+entry8+`]} {"reports":[]}`) + "\n" + good,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, malformed := ParseDraftMarkers(body)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ParseDraftMarkers = %+v, want only the valid marker's %+v", got, want)
			}
			if malformed != 1 {
				t.Errorf("malformed = %d, want 1 (the bad marker must be counted as dropped)", malformed)
			}
		})
	}
}

// TestParseDraftMarkers_InvalidEntryDropped pins per-entry validation: within
// a well-formed marker each invalid entry is dropped individually (the marker
// is not malformed) and the valid entry beside it survives.
func TestParseDraftMarkers_InvalidEntryDropped(t *testing.T) {
	valid := `{"id":"UR-issue-7","content_hash":"` + hashA + `"}`
	want := []MarkedReport{{ID: "UR-issue-7", ContentHash: hashA}}
	cases := map[string]MarkedReport{
		"UR-issue-0":           {ID: "UR-issue-0", ContentHash: hashB},
		"UR-issue-01":          {ID: "UR-issue-01", ContentHash: hashB},
		"UR-issue-+1":          {ID: "UR-issue-+1", ContentHash: hashB},
		"UR-issue- 1":          {ID: "UR-issue- 1", ContentHash: hashB},
		"UR-issue-1x":          {ID: "UR-issue-1x", ContentHash: hashB},
		"ur-issue-1":           {ID: "ur-issue-1", ContentHash: hashB},
		"UR-comment-1":         {ID: "UR-comment-1", ContentHash: hashB},
		"UR-comment-1-0":       {ID: "UR-comment-1-0", ContentHash: hashB},
		"UR-comment-0-1":       {ID: "UR-comment-0-1", ContentHash: hashB},
		"UR-comment-01-2":      {ID: "UR-comment-01-2", ContentHash: hashB},
		"UR-comment-1-02":      {ID: "UR-comment-1-02", ContentHash: hashB},
		"UR-unknown-1-2":       {ID: "UR-unknown-1-2", ContentHash: hashB},
		"63-char hash":         {ID: "UR-issue-8", ContentHash: strings.Repeat("b", 63)},
		"non-hex hash":         {ID: "UR-issue-8", ContentHash: strings.Repeat("g", 64)},
		"upper-case hex hash":  {ID: "UR-issue-8", ContentHash: strings.Repeat("B", 64)},
		"empty id and hash":    {},
		"65-char hash":         {ID: "UR-issue-8", ContentHash: strings.Repeat("b", 65)},
		"comment id, bad hash": {ID: "UR-comment-2-3", ContentHash: "zz"},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			body := marker(`{"reports":[` + valid + `,{"id":` + jsonString(bad.ID) + `,"content_hash":` + jsonString(bad.ContentHash) + `}]}`)
			got, malformed := ParseDraftMarkers(body)
			if !reflect.DeepEqual(got, want) || malformed != 0 {
				t.Errorf("ParseDraftMarkers = %+v malformed %d, want %+v malformed 0 (only the bad entry dropped)", got, malformed, want)
			}
		})
	}
}

// jsonString quotes s as a JSON string literal for hand-built payloads.
func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// TestParseDraftMarkers_ForeignPrefixIgnored: another Fishhawk marker carrying
// a comms-shaped payload yields nothing.
func TestParseDraftMarkers_ForeignPrefixIgnored(t *testing.T) {
	body := `<!-- fishhawk-intake:v1 {"reports":[{"id":"UR-issue-7","content_hash":"` + hashA + `"}]} -->`
	if got, malformed := ParseDraftMarkers(body); got != nil || malformed != 0 {
		t.Errorf("ParseDraftMarkers(foreign marker) = %+v malformed %d, want nil malformed 0", got, malformed)
	}
}

// TestContentHash pins the normalization ContentHash applies and the exact
// input string it hashes.
func TestContentHash(t *testing.T) {
	issue, comment := workmgmt.UserReportKindIssue, workmgmt.UserReportKindComment

	// Known answer: sha256 hex of "T\nB" (printf 'T\nB' | shasum -a 256).
	if got, want := ContentHash(issue, "T", "B"), "e1725446192b97dc9f6faa7e5c3ebe3960b960b7947a6c0981f69aa6f6af6ce6"; got != want {
		t.Errorf("ContentHash(issue, T, B) = %s, want %s", got, want)
	}
	if got := ContentHash(issue, "T", "B"); !contentHashRe.MatchString(got) {
		t.Errorf("ContentHash = %q, want 64 lowercase hex digits", got)
	}
	if ContentHash(issue, "Title", "line one\r\nline two") != ContentHash(issue, "Title", "line one\nline two") {
		t.Error("CRLF and LF bodies hash differently")
	}
	if ContentHash(issue, "  Title \n", "\n body \t\n") != ContentHash(issue, "Title", "body") {
		t.Error("surrounding whitespace on title or body changes the hash")
	}
	if ContentHash(issue, "Title", "the bug happens on save") == ContentHash(issue, "Title", "the bug happens on load") {
		t.Error("a material body edit does not change the hash")
	}
	if ContentHash(comment, "one title", "same body") != ContentHash(comment, "another title", "same body") {
		t.Error("a comment's hash depends on its title; a comment has none")
	}
	if ContentHash(issue, "one title", "same body") == ContentHash(issue, "another title", "same body") {
		t.Error("an issue's hash ignores its title")
	}
}
