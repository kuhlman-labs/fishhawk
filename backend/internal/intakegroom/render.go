package intakegroom

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// The rendered advisory section and its hidden machine-readable marker.
const (
	// SectionHeading opens the human-readable advisory section appended to a
	// filed body.
	SectionHeading = "### Intake signals (advisory)"

	// MarkerPrefix opens the hidden single-line marker carrying the compact
	// Signals JSON. It mirrors the existing fishhawk-fingerprint marker
	// convention in workmgmt/github/feedback.go: one HTML comment, one line,
	// a version token so a later shape change is distinguishable rather than
	// silently misparsed.
	MarkerPrefix = "<!-- fishhawk-intake:v1 "
	// MarkerSuffix closes the hidden marker.
	MarkerSuffix = " -->"
)

// advisoryPreamble states the posture in the body itself, where the reader
// of a filed issue sees it, rather than only in the API docs.
const advisoryPreamble = "Derived automatically when this item was filed. Everything below is a " +
	"candidate for a human: nothing was closed, relabelled or transitioned."

// escapeAngle replaces the angle brackets JSON leaves literal inside string
// values with their \u escapes.
//
// This is load-bearing, not decoration: a title containing "-->" would
// otherwise close the HTML comment early, spilling the rest of the payload
// into the rendered body and truncating the marker into something ParseBody
// cannot recover. Both characters only ever appear inside JSON string
// values here, so escaping them yields an equivalent document.
func escapeAngle(s string) string {
	s = strings.ReplaceAll(s, "<", `\u003c`)
	s = strings.ReplaceAll(s, ">", `\u003e`)
	return s
}

// marker renders the hidden machine-readable marker for s, or "" when the
// signals cannot be marshalled (which cannot happen for this closed shape,
// but is handled rather than panicked on inside a best-effort path).
func marker(s Signals) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return ""
	}
	payload := escapeAngle(strings.TrimSpace(buf.String()))
	return MarkerPrefix + payload + MarkerSuffix
}

// RenderBody returns body with the advisory section and hidden marker
// appended.
//
// It is a NO-OP when the signals are degraded and carry no findings: a
// filing whose grooming was unavailable gets a body byte-identical to the
// one it would have had before this feature existed. That keeps a
// degradation invisible on the write path it rides on, which is the whole
// posture of the hook (see DegradeReason).
func RenderBody(body string, s Signals) string {
	if s.Degraded && !s.HasFindings() {
		return body
	}

	var b strings.Builder
	b.WriteString(SectionHeading)
	b.WriteString("\n\n")
	b.WriteString(advisoryPreamble)
	b.WriteString("\n\n")

	b.WriteString("**Possible duplicates**\n")
	if len(s.Duplicates) == 0 {
		b.WriteString("- none found\n")
	}
	for _, d := range s.Duplicates {
		state := ""
		if d.Closed {
			state = ", closed"
		}
		fmt.Fprintf(&b, "- #%d %s — %s confidence (%.2f%s); shared: %s\n",
			d.Number, d.Title, d.Confidence, d.Score, state, basisOrNone(d.Basis))
	}

	b.WriteString("\n**Parent epic suggestion**\n")
	if s.EpicSuggestion == nil {
		b.WriteString("- none\n")
	} else {
		e := s.EpicSuggestion
		fmt.Fprintf(&b, "- #%d %s — %s confidence (%.2f); shared: %s\n",
			e.Number, e.Title, e.Confidence, e.Score, basisOrNone(e.Basis))
	}

	b.WriteString("\n**Provisional score**\n")
	if s.Score.Unscored {
		fmt.Fprintf(&b, "- unscored: %s\n", s.Score.CharterGap)
	} else {
		fmt.Fprintf(&b, "- %.1f, citing %s\n", s.Score.Value, strings.Join(citedIDs(s.Score), ", "))
		for _, c := range s.Score.Citations {
			fmt.Fprintf(&b, "  - **%s** — %s\n    (%s)\n", c.RubricID, c.Quote, c.Note)
		}
	}

	fmt.Fprintf(&b, "\nScanned %d existing item(s)", s.ScannedItems)
	if s.WindowTruncated {
		b.WriteString("; the scan window was truncated, so an older duplicate may be missed")
	}
	b.WriteString(".")
	if s.Degraded {
		fmt.Fprintf(&b, " Grooming degraded: %s.", s.DegradeReason)
	}
	b.WriteString("\n\n")
	b.WriteString(marker(s))
	b.WriteString("\n")

	section := b.String()
	if strings.TrimSpace(body) == "" {
		return section
	}
	return strings.TrimRight(body, "\n") + "\n\n" + section
}

// basisOrNone renders an empty shared-token basis explicitly rather than as
// a dangling separator.
func basisOrNone(basis string) string {
	if basis == "" {
		return "(no shared tokens)"
	}
	return basis
}

// citedIDs returns the rubric ids a score cites, in citation order.
func citedIDs(s Score) []string {
	out := make([]string, 0, len(s.Citations))
	for _, c := range s.Citations {
		out = append(out, c.RubricID)
	}
	return out
}

// markerRequiredFields are the payload keys marker() ALWAYS emits, because
// their Signals fields carry no omitempty. A payload missing any of them was
// not written by this package.
//
// They are checked by NAME rather than by decoded value because a
// json.Decoder fills only the fields a payload names: {} and
// {"scanned_items":0} decode into the same Signals, so an absent field is
// indistinguishable from a zero one after decoding.
var markerRequiredFields = []string{
	"score", "degraded", "scanned_items", "window_truncated", "duration_ms",
}

// ParseBody recovers the Signals from a body's hidden marker, so #2236's
// periodic sweep can read intake analysis back instead of redoing it.
//
// ACCEPTANCE IS EXACT, NOT BEST-EFFORT, because an issue body is mutable
// user-editable input and this marker is the input to a later grooming
// consumer. A marker is read back only when all four hold: the body carries a
// terminated marker; its payload is EXACTLY ONE complete JSON object with no
// second value and no trailing non-whitespace after it; that object names
// every field this package always emits and no field it does not know; and
// the decoded Signals satisfies validSignals. Anything else — a missing,
// unterminated, undecodable, truncated, trailing-data, incomplete or
// incoherent marker — returns ok=false and the ZERO Signals, never a partial
// value a caller could mistake for a real analysis.
func ParseBody(body string) (Signals, bool) {
	start := strings.LastIndex(body, MarkerPrefix)
	if start < 0 {
		return Signals{}, false
	}
	rest := body[start+len(MarkerPrefix):]
	end := strings.Index(rest, MarkerSuffix)
	if end < 0 {
		return Signals{}, false
	}
	payload := strings.TrimSpace(rest[:end])

	// Field-presence check, over the payload's raw key set.
	//
	// This deliberately uses a json.Decoder rather than json.Unmarshal:
	// Unmarshal rejects trailing input as a side effect, which would make
	// the explicit exactly-one-value check below unreachable and therefore
	// untestable. Rejecting trailing input stays that check's single job.
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(strings.NewReader(payload)).Decode(&fields); err != nil {
		return Signals{}, false
	}
	for _, f := range markerRequiredFields {
		if _, ok := fields[f]; !ok {
			return Signals{}, false
		}
	}

	var s Signals
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Signals{}, false
	}
	// EXACTLY ONE value. A Decoder stops at the end of the first value it
	// reads, so `{...} {...}` and `{...} junk` both decode without error —
	// the stream must be at EOF for the payload to be the single object
	// marker() wrote.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Signals{}, false
	}
	if !validSignals(s) {
		return Signals{}, false
	}
	return s, true
}

// validSignals reports whether s satisfies the invariants EVERY Signals this
// package renders satisfies.
//
// Structural decodability is not enough: a hand-edited marker can decode into
// a well-typed but incoherent Signals (a duplicate with no tracker number, a
// score both Unscored and citing, a degradation with no reason from the
// closed set), and a later consumer reading that back would be trusting
// tampered intake data. Each check below names a property a producer path in
// this package establishes, so none of them can reject a marker this package
// wrote.
func validSignals(s Signals) bool {
	// Counts and the summed advisory weight are never negative.
	if s.ScannedItems < 0 || s.DurationMS < 0 || s.Score.Value < 0 {
		return false
	}
	// Degrade and the hook set Degraded and DegradeReason together, from the
	// closed set — a degradation is always attributable to a named cause.
	if s.Degraded != (s.DegradeReason != "") {
		return false
	}
	if s.Degraded && !validDegradeReason(s.DegradeReason) {
		return false
	}
	// A gap is a finding: ScoreFiling reports Unscored with a stated gap
	// exactly when no citation survived, and never both a verdict and a
	// citation list.
	if s.Score.Unscored != (len(s.Score.Citations) == 0) {
		return false
	}
	if s.Score.Unscored && strings.TrimSpace(s.Score.CharterGap) == "" {
		return false
	}
	// No fabricated citation: every citation carries the rubric id it cites
	// and the charter line it quotes.
	for _, c := range s.Score.Citations {
		if strings.TrimSpace(c.RubricID) == "" || strings.TrimSpace(c.Quote) == "" {
			return false
		}
	}
	if len(s.Duplicates) > MaxDuplicates {
		return false
	}
	for _, d := range s.Duplicates {
		if !validCandidateFields(d.Number, d.Title, d.Score, d.Confidence) {
			return false
		}
	}
	if e := s.EpicSuggestion; e != nil {
		if !validCandidateFields(e.Number, e.Title, e.Score, e.Confidence) {
			return false
		}
	}
	return true
}

// validCandidateFields checks the fields a duplicate candidate and an epic
// suggestion share. Both are only ever emitted for a real scanned item: a
// tracker number is 1-based, a title that tokenizes to nothing scores 0 and
// is dropped before it becomes a candidate, and a similarity score reaching
// this point is in (0,1] with a band from the closed set.
func validCandidateFields(number int, title string, score float64, c Confidence) bool {
	if number <= 0 || strings.TrimSpace(title) == "" {
		return false
	}
	if score <= 0 || score > 1 {
		return false
	}
	return c == ConfidenceHigh || c == ConfidenceMedium || c == ConfidenceLow
}

// validDegradeReason reports whether reason is in the closed DegradeReason
// set, which DegradeReasons enumerates.
func validDegradeReason(reason DegradeReason) bool {
	for _, r := range DegradeReasons() {
		if r == reason {
			return true
		}
	}
	return false
}
