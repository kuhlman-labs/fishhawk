package fixupobligation

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestDetect_PositiveAcrossAllThreeSources: an obligation-bearing text on each
// routed channel is classified, tagged with its source, and given an id.
func TestDetect_PositiveAcrossAllThreeSources(t *testing.T) {
	got := Detect([]Source{
		{Kind: SourceConcern, Text: "Record the per-deletion counterfactual results in the PR body's ## Notes."},
		{Kind: SourceOperatorConcern, Text: "Also document the failure-mode mapping in the pull request description."},
		{Kind: SourceReason, Text: "Report the observed RED output in the run log."},
	})
	if len(got) != 3 {
		t.Fatalf("Detect returned %d obligations, want 3: %+v", len(got), got)
	}
	wantSources := []SourceKind{SourceConcern, SourceOperatorConcern, SourceReason}
	for i, ob := range got {
		if ob.Source != wantSources[i] {
			t.Errorf("obligation %d source = %q, want %q", i, ob.Source, wantSources[i])
		}
	}
}

// TestDetect_VerbWithoutSurfaceNounIsNotAnObligation is the named
// counterfactual vehicle for the classifier's CONJUNCTIVE requirement: delete
// the report-surface-noun half of IsObligation and this goes RED, because a
// routine recording instruction is then classified as a reporting obligation.
func TestDetect_VerbWithoutSurfaceNounIsNotAnObligation(t *testing.T) {
	for _, text := range []string{
		"Record the retry count in a metric so the dashboard shows it.",
		"Document the nil-pool guard with a comment next to the check.",
		"Report the error to the caller instead of swallowing it.",
	} {
		if IsObligation(text) {
			t.Errorf("IsObligation(%q) = true, want false (recording verb with NO report surface)", text)
		}
		if got := Detect([]Source{{Kind: SourceConcern, Text: text}}); len(got) != 0 {
			t.Errorf("Detect(%q) returned %+v, want none", text, got)
		}
	}
}

// TestDetect_SurfaceNounWithoutVerbIsNotAnObligation is the complementary
// counterfactual vehicle: delete the recording-verb half and this goes RED.
func TestDetect_SurfaceNounWithoutVerbIsNotAnObligation(t *testing.T) {
	for _, text := range []string{
		"The PR body claims the guard is tested but the diff shows otherwise.",
		"Your ## Notes section contradicts the test plan.",
		"The run log shows a wedged stage.",
	} {
		if IsObligation(text) {
			t.Errorf("IsObligation(%q) = true, want false (report surface with NO recording verb)", text)
		}
		if got := Detect([]Source{{Kind: SourceConcern, Text: text}}); len(got) != 0 {
			t.Errorf("Detect(%q) returned %+v, want none", text, got)
		}
	}
}

// TestDetect_RoutineConcernIsNotAnObligation: the overwhelmingly common
// routed-concern shape matches NEITHER half and yields nothing, so an ordinary
// fix-up renders a byte-identical prompt and emits no signal.
func TestDetect_RoutineConcernIsNotAnObligation(t *testing.T) {
	got := Detect([]Source{
		{Kind: SourceConcern, Text: "[high/correctness] Guard the nil pool in the retry path."},
		{Kind: SourceReason, Text: "route the two correctness concerns"},
	})
	if len(got) != 0 {
		t.Fatalf("Detect classified a routine fix-up as obligations: %+v", got)
	}
}

// TestDetect_IDsAreDeterministicAndKindOrdered: ids are ob-1..ob-N in the fixed
// concern → operator_concern → reason order REGARDLESS of the caller's
// interleaving. Determinism is load-bearing: the prompt renderer and the
// review-time re-derivation must mint the same id for the same instruction.
func TestDetect_IDsAreDeterministicAndKindOrdered(t *testing.T) {
	const (
		reasonText   = "Report the deletion table in the PR body."
		operatorText = "Document each dropped branch in the pull request description."
		concernA     = "Record counterfactual A in the PR body's ## Notes."
		concernB     = "Record counterfactual B in the PR body's ## Notes."
	)
	// Deliberately shuffled input order.
	got := Detect([]Source{
		{Kind: SourceReason, Text: reasonText},
		{Kind: SourceConcern, Text: concernA},
		{Kind: SourceOperatorConcern, Text: operatorText},
		{Kind: SourceConcern, Text: concernB},
	})
	if len(got) != 4 {
		t.Fatalf("Detect returned %d, want 4: %+v", len(got), got)
	}
	want := []Obligation{
		{ID: "ob-1", Source: SourceConcern, Text: concernA},
		{ID: "ob-2", Source: SourceConcern, Text: concernB},
		{ID: "ob-3", Source: SourceOperatorConcern, Text: operatorText},
		{ID: "ob-4", Source: SourceReason, Text: reasonText},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("obligation %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestDetect_OperatorConcernDedupedAgainstIdenticalConcernNote: since #2623 the
// free-text operator_concern is ALSO minted as a durable concern, so it arrives
// on both channels; one instruction must draw exactly one id.
func TestDetect_OperatorConcernDedupedAgainstIdenticalConcernNote(t *testing.T) {
	const text = "Record the per-deletion counterfactual results in the PR body's ## Notes."
	got := Detect([]Source{
		{Kind: SourceConcern, Text: text},
		{Kind: SourceOperatorConcern, Text: "  " + text + "  "}, // whitespace-differing duplicate
	})
	if len(got) != 1 {
		t.Fatalf("Detect returned %d obligations, want 1 (operator_concern deduped): %+v", len(got), got)
	}
	if got[0].Source != SourceConcern || got[0].ID != "ob-1" {
		t.Errorf("surviving obligation = %+v, want the ob-1 concern", got[0])
	}
}

// TestDetect_EmptyAndWhitespaceSourcesSkipped: an absent operator_concern /
// reason arrives as "" and must not be classified or draw an id.
func TestDetect_EmptyAndWhitespaceSourcesSkipped(t *testing.T) {
	got := Detect([]Source{
		{Kind: SourceOperatorConcern, Text: ""},
		{Kind: SourceReason, Text: "   \n\t "},
		{Kind: SourceConcern, Text: "Record the deletion table in the PR body."},
	})
	if len(got) != 1 || got[0].ID != "ob-1" {
		t.Fatalf("Detect = %+v, want exactly the one real obligation as ob-1", got)
	}
}

// TestBound_TruncatesAtCapOnARuneBoundary: a 4000-byte operator_concern cannot
// swamp the prompt or the audit payload, and the cut never splits a rune.
func TestBound_TruncatesAtCapOnARuneBoundary(t *testing.T) {
	if got := Bound("short"); got != "short" {
		t.Errorf("Bound(short) = %q, want unchanged", got)
	}
	// Multi-byte runes straddling the cap prove the rune-boundary walk.
	long := strings.Repeat("é", MaxExcerptBytes)
	got := Bound(long)
	if !strings.HasSuffix(got, truncationMarker) {
		t.Errorf("Bound did not mark truncation: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("Bound produced invalid UTF-8: %q", got)
	}
	if body := strings.TrimSuffix(got, truncationMarker); len(body) > MaxExcerptBytes {
		t.Errorf("bounded body is %d bytes, want <= %d", len(body), MaxExcerptBytes)
	}
}

// TestDetect_BoundsTheStoredExcerpt: Detect stores the BOUNDED text.
func TestDetect_BoundsTheStoredExcerpt(t *testing.T) {
	long := "Record this in the PR body's ## Notes. " + strings.Repeat("x", 5000)
	got := Detect([]Source{{Kind: SourceConcern, Text: long}})
	if len(got) != 1 {
		t.Fatalf("Detect = %+v, want 1", got)
	}
	if len(got[0].Text) > MaxExcerptBytes+len(truncationMarker) {
		t.Errorf("stored excerpt is %d bytes, want bounded", len(got[0].Text))
	}
}

func declaredPair() []Obligation {
	return []Obligation{
		{ID: "ob-1", Source: SourceConcern, Text: "Record counterfactual A in the PR body."},
		{ID: "ob-2", Source: SourceReason, Text: "Record counterfactual B in the PR body."},
	}
}

// TestUndelivered_AllMetReturnsNothing is the pure half of acceptance criterion
// 3's no-noise control: every declared id carries a `met` report → no finding.
func TestUndelivered_AllMetReturnsNothing(t *testing.T) {
	got := Undelivered(declaredPair(), []Report{
		{ID: "ob-1", Status: StatusMet},
		{ID: "ob-2", Status: StatusMet},
	})
	if len(got) != 0 {
		t.Fatalf("Undelivered = %+v, want none when every obligation is met", got)
	}
}

// TestUndelivered_NoReportsReturnsEveryDeclaredAsUnreported: an absent sidecar
// (or one whose entries the runner dropped) leaves every obligation unreported.
func TestUndelivered_NoReportsReturnsEveryDeclaredAsUnreported(t *testing.T) {
	got := Undelivered(declaredPair(), nil)
	if len(got) != 2 {
		t.Fatalf("Undelivered = %+v, want both declared obligations", got)
	}
	for i, f := range got {
		if f.Status != StatusUnreported {
			t.Errorf("finding %d status = %q, want %q", i, f.Status, StatusUnreported)
		}
	}
	if got[0].ID != "ob-1" || got[1].ID != "ob-2" {
		t.Errorf("Undelivered did not preserve declared order: %+v", got)
	}
}

// TestUndelivered_DeclinedCarriesStatus: an honest decline is still undelivered,
// but it is reported as a decline rather than a silence so the operator can tell
// a refusal from nothing at all. The agent's stated REASON is deliberately not
// part of the finding — it is validated on the runner and never transmitted
// (#2737 security fix-up), so a Finding carries only backend-derived text.
func TestUndelivered_DeclinedCarriesStatus(t *testing.T) {
	got := Undelivered(declaredPair(), []Report{
		{ID: "ob-1", Status: StatusMet},
		{ID: "ob-2", Status: StatusDeclined},
	})
	if len(got) != 1 {
		t.Fatalf("Undelivered = %+v, want only the declined obligation", got)
	}
	if got[0].ID != "ob-2" || got[0].Status != StatusDeclined {
		t.Errorf("finding = %+v, want ob-2 declined", got[0])
	}
	// The instruction excerpt on the finding is the OPERATOR's, carried from the
	// declared obligation — never anything the agent wrote.
	if got[0].Text != "Record counterfactual B in the PR body." {
		t.Errorf("declined finding lost the operator's instruction excerpt: %q", got[0].Text)
	}
}

// TestUndelivered_UnknownReportIDIgnored: a report naming an id that was never
// declared satisfies nothing and produces no finding of its own.
func TestUndelivered_UnknownReportIDIgnored(t *testing.T) {
	got := Undelivered(declaredPair(), []Report{
		{ID: "ob-9", Status: StatusMet},
		{ID: "ob-1", Status: StatusMet},
	})
	if len(got) != 1 || got[0].ID != "ob-2" {
		t.Fatalf("Undelivered = %+v, want only ob-2 (ob-9 ignored, ob-1 met)", got)
	}
}

// TestUndelivered_NothingDeclaredReturnsNothing: the no-obligation fix-up — the
// common case — can never produce a finding, whatever the agent reported.
func TestUndelivered_NothingDeclaredReturnsNothing(t *testing.T) {
	if got := Undelivered(nil, []Report{{ID: "ob-1", Status: StatusDeclined}}); len(got) != 0 {
		t.Fatalf("Undelivered = %+v, want none when nothing was declared", got)
	}
}

// TestUndelivered_DuplicateReportCannotUpgradeADecline: a second entry for the
// same id must not let a `met` overwrite an earlier `declined`.
func TestUndelivered_DuplicateReportCannotUpgradeADecline(t *testing.T) {
	got := Undelivered([]Obligation{{ID: "ob-1", Source: SourceConcern, Text: "Record it in the PR body."}},
		[]Report{
			{ID: "ob-1", Status: StatusDeclined},
			{ID: "ob-1", Status: StatusMet},
		})
	if len(got) != 1 || got[0].Status != StatusDeclined {
		t.Fatalf("Undelivered = %+v, want the first (declined) report to stand", got)
	}
}

// TestDetect_CarriesUntrustedProvenance pins the trust-provenance carry-through
// the #2737 security fix-up added. A routed concern note can be
// acceptance-SYNTHESIZED — attacker-influenceable free text an automated
// validator authored (ADR-050 / E31.8 / #1613) — and the render sites decide
// whether to quarantine an excerpt solely from this flag. Detect dropping it
// would launder untrusted text into trusted prompt content at both sites.
//
// The two arms are SELF-PAIRED: byte-identical instruction text, differing only
// in the caller-supplied Untrusted flag, so the assertion cannot pass on a text
// comparison that would hold with or without the carry-through.
func TestDetect_CarriesUntrustedProvenance(t *testing.T) {
	const note = "Record the per-deletion counterfactual results in the PR body's ## Notes."

	untrusted := Detect([]Source{{Kind: SourceConcern, Text: note, Untrusted: true}})
	if len(untrusted) != 1 {
		t.Fatalf("untrusted arm = %+v, want one obligation", untrusted)
	}
	if !untrusted[0].Untrusted {
		t.Errorf("obligation = %+v, want Untrusted true carried from the source", untrusted[0])
	}

	trusted := Detect([]Source{{Kind: SourceConcern, Text: note}})
	if len(trusted) != 1 {
		t.Fatalf("trusted arm = %+v, want one obligation", trusted)
	}
	if trusted[0].Untrusted {
		t.Errorf("obligation = %+v, want Untrusted false for operator-authored text", trusted[0])
	}
	// Discrimination: everything but the flag is identical between the arms.
	if untrusted[0].ID != trusted[0].ID || untrusted[0].Text != trusted[0].Text {
		t.Fatalf("the arms differ in more than the flag (%+v vs %+v) — the assertion is not isolating provenance",
			untrusted[0], trusted[0])
	}

	// The operator_concern dedupe must retain the CONCERN's flag, never
	// silently upgrade the surviving obligation to trusted.
	deduped := Detect([]Source{
		{Kind: SourceConcern, Text: note, Untrusted: true},
		{Kind: SourceOperatorConcern, Text: note},
	})
	if len(deduped) != 1 {
		t.Fatalf("deduped = %+v, want exactly one obligation", deduped)
	}
	if !deduped[0].Untrusted {
		t.Errorf("deduped obligation = %+v, want the surviving concern's Untrusted flag retained", deduped[0])
	}
}
