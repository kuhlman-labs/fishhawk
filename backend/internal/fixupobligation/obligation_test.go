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
	// Every text here names a PR-body surface in prose, but no source declares
	// a target, so each obligation is a code-target obligation with PRBody
	// false (E83.37 / #4022: prose never decides satisfiability). The id
	// ordering itself is what this pins.
	want := []Obligation{
		{ID: "ob-1", Source: SourceConcern, Text: concernA, Target: TargetCode},
		{ID: "ob-2", Source: SourceConcern, Text: concernB, Target: TargetCode},
		{ID: "ob-3", Source: SourceOperatorConcern, Text: operatorText, Target: TargetCode},
		{ID: "ob-4", Source: SourceReason, Text: reasonText, Target: TargetCode},
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

// TestDetect_TargetDecidesSatisfiability pins E83.37 / #4022: PRBody — the
// flag that makes an obligation unsatisfiable — is derived from the
// ROUTER-DECLARED target alone, never from the instruction's prose.
//
//   - prose naming the PR body + target code -> an obligation, PRBody false;
//   - the same prose with NO target -> PRBody false (absent = code);
//   - prose naming NO surface (and no recording verb) + target pr_body -> still
//     an obligation, PRBody true;
//   - a reporting instruction + target docs -> PRBody false (docs is writable).
//
// Counterfactuals: hard-code PRBody false in Detect and the pr_body rows go
// RED; re-add a prose disjunct (PRBody also true when the text names "pr body")
// and the code / no-target rows go RED; drop the `target != TargetPRBody`
// inclusion disjunct and the no-surface pr_body row (which carries no recording
// verb, so IsObligation is false) is not declared at all — RED.
func TestDetect_TargetDecidesSatisfiability(t *testing.T) {
	const prBodyProse = "Record the counterfactual results in the PR body's ## Notes."
	cases := []struct {
		name       string
		text       string
		target     Target
		wantTarget Target
		wantPRBody bool
	}{
		{"pr_body_prose_code_target", prBodyProse, TargetCode, TargetCode, false},
		{"pr_body_prose_no_target", prBodyProse, "", TargetCode, false},
		{"no_surface_prose_pr_body_target", "Tighten the summary to mention the retry bound.", TargetPRBody, TargetPRBody, true},
		{"reporting_prose_pr_body_target", prBodyProse, TargetPRBody, TargetPRBody, true},
		{"run_log_prose_docs_target", "Report the observed RED output in the run log.", TargetDocs, TargetDocs, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Detect([]Source{{Kind: SourceConcern, Text: tc.text, Target: tc.target}})
			if len(got) != 1 {
				t.Fatalf("Detect(%q, target %q) = %+v, want exactly one obligation", tc.text, tc.target, got)
			}
			if got[0].PRBody != tc.wantPRBody {
				t.Errorf("Detect(%q, target %q).PRBody = %v, want %v", tc.text, tc.target, got[0].PRBody, tc.wantPRBody)
			}
			if got[0].Target != tc.wantTarget {
				t.Errorf("Detect(%q, target %q).Target = %q, want %q", tc.text, tc.target, got[0].Target, tc.wantTarget)
			}
		})
	}
}

// TestDetect_NoTargetNonReportingInstructionStaysUndeclared is the no-noise
// control for the pr_body inclusion disjunct: an ordinary code instruction
// with no recording verb and a code (or absent) target is NOT declared, so the
// disjunct widens the set only for a router-declared pr_body target.
func TestDetect_NoTargetNonReportingInstructionStaysUndeclared(t *testing.T) {
	for _, target := range []Target{"", TargetCode, TargetDocs} {
		if got := Detect([]Source{{Kind: SourceReason, Text: "Tighten the summary to mention the retry bound.", Target: target}}); got != nil {
			t.Errorf("Detect(target %q) = %+v, want nil", target, got)
		}
	}
}

// TestParseTarget accepts exactly the three literals and refuses everything
// else, the empty string included (absence is the caller's to map to code).
func TestParseTarget(t *testing.T) {
	for _, ok := range []string{"code", "pr_body", "docs"} {
		if got, valid := ParseTarget(ok); !valid || string(got) != ok {
			t.Errorf("ParseTarget(%q) = (%q, %v), want (%q, true)", ok, got, valid, ok)
		}
	}
	for _, bad := range []string{"", "PR_BODY", "pr-body", "prbody", " code", "tests"} {
		if got, valid := ParseTarget(bad); valid || got != "" {
			t.Errorf("ParseTarget(%q) = (%q, %v), want (\"\", false)", bad, got, valid)
		}
	}
}

// TestUndelivered_PRBodyObligation_UnsatisfiableEvenWhenMetReported is the
// heart of #2782: a PR-body obligation is returned with StatusUnsatisfiable
// EVEN when the agent reported it `met`, because the pass has no mechanism to
// write the PR body so a `met` claim cannot be true.
//
// The `met` report is seeded BY CONSTRUCTION — a literal Report{ID:"ob-1",
// Status:StatusMet} — never produced by calling the control, so deleting the
// PRBody short-circuit in Undelivered lands the RED on the status assertion
// (the obligation then follows the `met`-suppresses-the-finding path and
// vanishes) rather than on any fixture setup.
func TestUndelivered_PRBodyObligation_UnsatisfiableEvenWhenMetReported(t *testing.T) {
	declared := []Obligation{
		{ID: "ob-1", Source: SourceConcern, Text: "Record the counterfactual table in the PR body's ## Notes.", PRBody: true},
	}
	got := Undelivered(declared, []Report{{ID: "ob-1", Status: StatusMet}})
	if len(got) != 1 {
		t.Fatalf("Undelivered = %+v, want the PR-body obligation returned despite a met report", got)
	}
	if got[0].ID != "ob-1" || got[0].Status != StatusUnsatisfiable {
		t.Errorf("finding = %+v, want ob-1 with status %q", got[0], StatusUnsatisfiable)
	}
}

// TestUndelivered_RunLogObligation_MetSuppressesFinding proves the
// classification is NARROW rather than blanket: an obligation naming the run
// log — a surface the pass CAN write — keeps the prior `met`-suppresses-the-
// finding behaviour, so a satisfied run-log obligation emits nothing.
func TestUndelivered_RunLogObligation_MetSuppressesFinding(t *testing.T) {
	declared := Detect([]Source{{Kind: SourceConcern, Text: "Report the observed RED output in the run log."}})
	if len(declared) != 1 || declared[0].PRBody {
		t.Fatalf("run-log obligation misclassified: %+v", declared)
	}
	if got := Undelivered(declared, []Report{{ID: "ob-1", Status: StatusMet}}); len(got) != 0 {
		t.Fatalf("Undelivered = %+v, want nothing — a met run-log obligation is satisfiable and suppressed", got)
	}
}

// TestUndelivered_TargetDecidesUnsatisfiable is the done-means at the join
// (E83.37 / #4022): the SAME PR-body prose, reported `met`, is suppressed when
// its router targeted code (nil — a prose mention is not a structural
// impossibility) and returned unsatisfiable when its router targeted pr_body.
// Both obligations come from Detect, so the flag is the real derivation.
func TestUndelivered_TargetDecidesUnsatisfiable(t *testing.T) {
	const text = "Record the counterfactual table in the PR body's ## Notes."
	met := []Report{{ID: "ob-1", Status: StatusMet}}

	code := Detect([]Source{{Kind: SourceReason, Text: text, Target: TargetCode}})
	if got := Undelivered(code, met); got != nil {
		t.Errorf("code target + met: Undelivered = %+v, want nil", got)
	}
	prBody := Detect([]Source{{Kind: SourceReason, Text: text, Target: TargetPRBody}})
	got := Undelivered(prBody, met)
	if len(got) != 1 || got[0].Status != StatusUnsatisfiable {
		t.Errorf("pr_body target + met: Undelivered = %+v, want one %q finding", got, StatusUnsatisfiable)
	}
}
