package planreview

import (
	"strings"
	"testing"
)

// TestDecodeVerdict_RegexEscapeInFreeForm covers the originating bug (#739): a
// verdict whose free_form quotes a regex containing `\-` is invalid JSON under
// a strict decode, but the sanitizing retry rescues it and preserves the regex
// text verbatim.
func TestDecodeVerdict_RegexEscapeInFreeForm(t *testing.T) {
	const regex = `ghs_[A-Za-z0-9_.\-]{36,}`
	// The raw bytes the model emits: a lone `\-` inside the JSON string, which
	// Go's encoding/json rejects as "invalid character ... in string escape".
	raw := []byte(`{"verdict":"reject","free_form":"redact ` + regex + `"}`)

	verdict, err := DecodeVerdict(raw)
	if err != nil {
		t.Fatalf("DecodeVerdict: %v", err)
	}
	if verdict.Verdict != VerdictReject {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, VerdictReject)
	}
	if !strings.Contains(verdict.FreeForm, regex) {
		t.Errorf("FreeForm = %q, want it to contain the regex %q verbatim", verdict.FreeForm, regex)
	}
}

// TestDecodeVerdict_OtherInvalidEscapesInConcern covers the sibling escapes
// `\d`, `\w`, `\.` embedded in a concern note — all of which are valid in a
// Go/regex context but illegal JSON escapes.
func TestDecodeVerdict_OtherInvalidEscapesInConcern(t *testing.T) {
	const note = `match \d+ \w* on a \. boundary`
	raw := []byte(`{"verdict":"approve_with_concerns","concerns":[{"severity":"low","category":"style","note":"` + note + `"}]}`)

	verdict, err := DecodeVerdict(raw)
	if err != nil {
		t.Fatalf("DecodeVerdict: %v", err)
	}
	if verdict.Verdict != VerdictApproveWithConcerns {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, VerdictApproveWithConcerns)
	}
	if len(verdict.Concerns) != 1 {
		t.Fatalf("len(Concerns) = %d, want 1", len(verdict.Concerns))
	}
	if !strings.Contains(verdict.Concerns[0].Note, note) {
		t.Errorf("Concern note = %q, want it to contain %q verbatim", verdict.Concerns[0].Note, note)
	}
}

// TestDecodeVerdict_WellFormedRoundTrips asserts a verdict with legitimate JSON
// escapes (\n, \", a doubled backslash, and \uXXXX) decodes through the strict
// path with every escaped value intact and uncorrupted by the sanitizer.
func TestDecodeVerdict_WellFormedRoundTrips(t *testing.T) {
	// raw carries: \n (newline), \" (quote), \\ (one literal backslash), and
	// é (the unicode escape for é). All are legal JSON escapes the
	// sanitizer must leave intact — the strict path should decode them.
	raw := []byte(`{"verdict":"approve","free_form":"line1\nsay \"hi\" path C:\\dir don\u00e9"}`)

	verdict, err := DecodeVerdict(raw)
	if err != nil {
		t.Fatalf("DecodeVerdict: %v", err)
	}
	if verdict.Verdict != VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, VerdictApprove)
	}
	want := "line1\nsay \"hi\" path C:\\dir doné"
	if verdict.FreeForm != want {
		t.Errorf("FreeForm = %q, want %q (escapes must round-trip uncorrupted)", verdict.FreeForm, want)
	}
}

// TestDecodeVerdict_MalformedReturnsOriginalError asserts genuinely-malformed
// JSON (a truncated object the sanitizer cannot rescue) still returns a decode
// error — the original strict error, not a masked one.
func TestDecodeVerdict_MalformedReturnsOriginalError(t *testing.T) {
	raw := []byte(`{"verdict":"approve"`) // truncated: no closing brace

	_, err := DecodeVerdict(raw)
	if err == nil {
		t.Fatal("expected a decode error from truncated JSON, got nil")
	}
}

// TestDecodeVerdict_JSONFencedDecodes covers the originating bug (#889): a
// verdict wrapped in a ```json … ``` markdown fence (as reviewer models
// commonly emit) decodes to the same ReviewVerdict as the unfenced form,
// rather than failing strict decode on the leading backtick.
func TestDecodeVerdict_JSONFencedDecodes(t *testing.T) {
	const inner = `{"verdict":"approve","free_form":"looks good"}`
	raw := []byte("```json\n" + inner + "\n```")

	verdict, err := DecodeVerdict(raw)
	if err != nil {
		t.Fatalf("DecodeVerdict: %v", err)
	}
	if verdict.Verdict != VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, VerdictApprove)
	}
	if verdict.FreeForm != "looks good" {
		t.Errorf("FreeForm = %q, want %q", verdict.FreeForm, "looks good")
	}
}

// TestDecodeVerdict_BareFencedDecodes covers a verdict wrapped in a bare ```
// fence with no info string; it must decode identically to the unfenced form.
func TestDecodeVerdict_BareFencedDecodes(t *testing.T) {
	const inner = `{"verdict":"reject","free_form":"missing tests"}`
	raw := []byte("```\n" + inner + "\n```")

	verdict, err := DecodeVerdict(raw)
	if err != nil {
		t.Fatalf("DecodeVerdict: %v", err)
	}
	if verdict.Verdict != VerdictReject {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, VerdictReject)
	}
	if verdict.FreeForm != "missing tests" {
		t.Errorf("FreeForm = %q, want %q", verdict.FreeForm, "missing tests")
	}
}

// TestDecodeVerdict_FencedWithIllegalEscape asserts the fence-strip pre-step
// and the escape-repair retry compose: a fenced verdict whose body ALSO
// contains an illegal `\-` escape still round-trips after de-fencing, via the
// sanitize retry.
func TestDecodeVerdict_FencedWithIllegalEscape(t *testing.T) {
	const regex = `ghs_[A-Za-z0-9_.\-]{36,}`
	raw := []byte("```json\n" + `{"verdict":"reject","free_form":"redact ` + regex + `"}` + "\n```")

	verdict, err := DecodeVerdict(raw)
	if err != nil {
		t.Fatalf("DecodeVerdict: %v", err)
	}
	if verdict.Verdict != VerdictReject {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, VerdictReject)
	}
	if !strings.Contains(verdict.FreeForm, regex) {
		t.Errorf("FreeForm = %q, want it to contain the regex %q verbatim", verdict.FreeForm, regex)
	}
}

// TestDecodeVerdict_UnicodeEscapeSurvivesSanitizer exercises the sanitizer's
// `\uXXXX` branch (decode.go), which the strict-path round-trip test does not
// reach. The input fails the strict decode (a lone `\-`), forcing the
// sanitizing retry; a legitimate `é` in the SAME string must be consumed
// intact (decoded to é), not doubled, while the `\-` is doubled to a literal.
func TestDecodeVerdict_UnicodeEscapeSurvivesSanitizer(t *testing.T) {
	// Raw model bytes: a valid é (é) and an illegal \- in one string.
	raw := []byte(`{"verdict":"approve","free_form":"café matches \- here"}`)

	verdict, err := DecodeVerdict(raw)
	if err != nil {
		t.Fatalf("DecodeVerdict: %v", err)
	}
	if verdict.Verdict != VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, VerdictApprove)
	}
	// é must have decoded to é (the sanitizer left the unicode escape
	// intact rather than doubling its backslash).
	if !strings.Contains(verdict.FreeForm, "café") {
		t.Errorf("FreeForm = %q, want it to contain decoded unicode 'café'", verdict.FreeForm)
	}
	// The illegal \- must survive as a literal backslash-dash.
	if !strings.Contains(verdict.FreeForm, `\-`) {
		t.Errorf("FreeForm = %q, want it to contain literal \\-", verdict.FreeForm)
	}
}

// TestDecodeVerdict_ConcernResolutions_StrictPath asserts a verdict
// carrying the #984 concern_resolutions array decodes on the strict path
// with every member intact — the delta-verification re-review's wire
// shape.
func TestDecodeVerdict_ConcernResolutions_StrictPath(t *testing.T) {
	raw := []byte(`{"verdict":"approve_with_concerns","concerns":[{"severity":"low","category":"scope","note":"new drift"}],"concern_resolutions":[{"id":"11111111-1111-1111-1111-111111111111","resolution":"confirmed","note":"fix lands"},{"id":"22222222-2222-2222-2222-222222222222","resolution":"reopened"}]}`)

	verdict, err := DecodeVerdict(raw)
	if err != nil {
		t.Fatalf("DecodeVerdict: %v", err)
	}
	if len(verdict.ConcernResolutions) != 2 {
		t.Fatalf("ConcernResolutions = %d entries, want 2", len(verdict.ConcernResolutions))
	}
	if verdict.ConcernResolutions[0].ID != "11111111-1111-1111-1111-111111111111" ||
		verdict.ConcernResolutions[0].Resolution != "confirmed" ||
		verdict.ConcernResolutions[0].Note != "fix lands" {
		t.Errorf("first resolution = %+v, want confirmed with note", verdict.ConcernResolutions[0])
	}
	if verdict.ConcernResolutions[1].Resolution != "reopened" || verdict.ConcernResolutions[1].Note != "" {
		t.Errorf("second resolution = %+v, want reopened with empty note", verdict.ConcernResolutions[1])
	}
	if len(verdict.Concerns) != 1 {
		t.Errorf("Concerns = %d entries, want 1 (resolutions must not displace new findings)", len(verdict.Concerns))
	}
}

// TestDecodeVerdict_ConcernResolutions_SurvivesFenceAndEscapeRepair
// asserts the resolutions array rides through the fence-strip + escape
// sanitize repair path unchanged — a fenced verdict with an illegal
// escape elsewhere must not drop the #984 field.
func TestDecodeVerdict_ConcernResolutions_SurvivesFenceAndEscapeRepair(t *testing.T) {
	inner := `{"verdict":"approve","free_form":"matches \- here","concern_resolutions":[{"id":"33333333-3333-3333-3333-333333333333","resolution":"superseded"}]}`
	raw := []byte("```json\n" + inner + "\n```")

	verdict, err := DecodeVerdict(raw)
	if err != nil {
		t.Fatalf("DecodeVerdict: %v", err)
	}
	if len(verdict.ConcernResolutions) != 1 {
		t.Fatalf("ConcernResolutions = %d entries, want 1", len(verdict.ConcernResolutions))
	}
	if verdict.ConcernResolutions[0].Resolution != "superseded" {
		t.Errorf("resolution = %q, want superseded", verdict.ConcernResolutions[0].Resolution)
	}
}

// TestDecodeVerdict_NoConcernResolutions_YieldsNil asserts a
// resolutions-free verdict (every pre-#984 reviewer's output) decodes
// with a nil ConcernResolutions — the additive-field contract.
func TestDecodeVerdict_NoConcernResolutions_YieldsNil(t *testing.T) {
	raw := []byte(`{"verdict":"approve","free_form":"clean"}`)

	verdict, err := DecodeVerdict(raw)
	if err != nil {
		t.Fatalf("DecodeVerdict: %v", err)
	}
	if verdict.ConcernResolutions != nil {
		t.Errorf("ConcernResolutions = %+v, want nil for a resolutions-free verdict", verdict.ConcernResolutions)
	}
}
