package mcpserver

import (
	"reflect"
	"strings"
	"testing"
)

// audit_check_wording_test.go is the E64.44 / #3161 done-means test.
//
// Fishhawk PUBLISHES a `fishhawk_audit_complete` Check Run on every run's pull
// request. Whether that check gates the merge is a property of the repository's
// branch protection, not of Fishhawk — and on this very repository the check was
// NOT required for the whole dogfood period while four operator-facing MCP
// surfaces asserted it was. This file pins the corrected wording against the
// SHIPPED strings, so a comment-only or otherwise no-op touch of a swept file
// cannot satisfy the scope-completeness gate while leaving the claim standing
// (the #1169 gap).
//
// It asserts on OUTPUT, never on source text: the registered tool descriptions
// walked over a real in-memory MCP session (the same registration path
// tools_test.go's listToolDescriptions uses), the `mergeRunNote` constant that
// rides on every fishhawk_merge_run response, the string
// implementReviewMergeHint actually returns, the `merge_pr` next-action
// Precondition mergeRunAction actually builds, and the wire-visible jsonschema
// descriptions on the two struct fields that carry the claim.

// bannedAuditCheckClaims are the over-claims the sweep removed. Each is a
// substring match on the normalized (lower-cased, whitespace-collapsed) shipped
// string, so a reflow or a line re-wrap cannot smuggle one back in.
//
// The rationale, not just the phrasing, is what these ban: "required
// fishhawk_audit_complete" asserts a forge fact Fishhawk never read, and
// "branch protection blocks the merge" asserts it unconditionally.
var bannedAuditCheckClaims = []struct {
	pattern string
	why     string
}{
	{
		pattern: "required fishhawk_audit_complete",
		why:     "Fishhawk publishes the check; it does not make it required. Whether it is required is a repo property fishhawk_doctor's merge_gate rung reports (#3161).",
	},
	{
		pattern: "required review + the fishhawk_audit_complete",
		why:     "the pre-#3161 mergeRunNote phrasing, which named a branch-protection composition nothing had read.",
	},
	{
		pattern: "the fishhawk_audit_complete check is required",
		why:     "same over-claim in predicate form (#3161).",
	},
	{
		pattern: "branch protection blocks the merge",
		why:     "unconditional: the check may not be required at all, and even a required check can carry bypass entries (#3161).",
	},
}

// requiredConditionalClaim is the positive half of the done-means. A surface
// that merely DELETES the word "required" has not told the operator anything;
// each swept surface must point at the reconciliation that answers the question
// it stopped over-claiming. Asserting a positive obligation is what makes a
// no-op or deletion-only touch fail this test rather than pass it.
const requiredConditionalClaim = "merge_gate"

// normalizeAuditWording lower-cases and collapses whitespace runs so a multi-
// line description or a re-wrapped comment is compared on its words, not its
// line breaks. Mirrors normalizeDescription in tools_test.go.
func normalizeAuditWording(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// auditWordingSurface is one shipped string plus whether it must carry the
// positive conditional claim. Every surface is banned-pattern-checked; the
// four the sweep rewrote must ALSO name the merge_gate rung.
type auditWordingSurface struct {
	name            string
	text            string
	mustBeCondition bool
}

// sweptAuditWordingSurfaces builds the shipped strings this test asserts on.
func sweptAuditWordingSurfaces(t *testing.T) []auditWordingSurface {
	t.Helper()

	statusField, ok := reflect.TypeOf(GetRunStatusOutput{}).FieldByName("ImplementReviewMergeHint")
	if !ok {
		t.Fatal("GetRunStatusOutput has no ImplementReviewMergeHint field — the #3161 swept surface moved; re-point this test rather than deleting it")
	}
	noteField, ok := reflect.TypeOf(MergeRunOutput{}).FieldByName("Note")
	if !ok {
		t.Fatal("MergeRunOutput has no Note field — the #3161 swept surface moved; re-point this test rather than deleting it")
	}

	hint := implementReviewMergeHint(&ReviewStatus{Stage: "implement", Status: "pending"})
	if hint == "" {
		t.Fatal("implementReviewMergeHint returned empty for a pending review — the swept surface is unreachable, so this test would vacuously pass")
	}
	precondition := mergeRunAction(&Run{ID: "11111111-1111-1111-1111-111111111111"}).Precondition
	if precondition == "" {
		t.Fatal("mergeRunAction Precondition is empty — the swept surface is unreachable, so this test would vacuously pass")
	}

	return []auditWordingSurface{
		{name: "mergeRunNote", text: mergeRunNote, mustBeCondition: true},
		{name: "implementReviewMergeHint(pending)", text: hint, mustBeCondition: true},
		{name: "mergeRunAction().Precondition", text: precondition, mustBeCondition: true},
		{name: "GetRunStatusOutput.ImplementReviewMergeHint jsonschema", text: statusField.Tag.Get("jsonschema"), mustBeCondition: true},
		{name: "MergeRunOutput.Note jsonschema", text: noteField.Tag.Get("jsonschema"), mustBeCondition: true},
	}
}

// TestAuditCheckWording_NoSurfaceClaimsTheCheckIsRequired is the done-means
// assertion for the #3161 wording sweep. It fails if any shipped operator-facing
// string asserts `fishhawk_audit_complete` is a required check or that branch
// protection unconditionally blocks the merge, and it fails if a swept surface
// stops pointing at the merge_gate rung that answers the question honestly.
func TestAuditCheckWording_NoSurfaceClaimsTheCheckIsRequired(t *testing.T) {
	surfaces := sweptAuditWordingSurfaces(t)

	// Every registered tool description is swept too: the claim rode on
	// fishhawk_merge_run's description body, and nothing stops a future tool
	// from repeating it. These are banned-pattern-checked only — a tool that
	// never mentions the check has nothing to condition.
	for name, desc := range listToolDescriptions(t) {
		surfaces = append(surfaces, auditWordingSurface{name: "tool description " + name, text: desc})
	}

	for _, surface := range surfaces {
		norm := normalizeAuditWording(surface.text)
		if norm == "" {
			t.Errorf("%s: shipped string is empty — nothing to assert on", surface.name)
			continue
		}
		for _, banned := range bannedAuditCheckClaims {
			if strings.Contains(norm, banned.pattern) {
				t.Errorf("%s ships the over-claim %q: %s\n\nfull text: %s",
					surface.name, banned.pattern, banned.why, surface.text)
			}
		}
		if surface.mustBeCondition && !strings.Contains(norm, requiredConditionalClaim) {
			t.Errorf("%s no longer names the %q rung that reports what the forge actually enforces — "+
				"dropping the over-claim without pointing at the reconciliation leaves the operator with less, not more (#3161)\n\nfull text: %s",
				surface.name, requiredConditionalClaim, surface.text)
		}
	}
}

// TestAuditCheckWording_SweptSurfacesNameThePublishedCheck pins the other half
// of the corrected claim: each swept surface still NAMES the check by its wire
// name, so the sweep replaced the false statement rather than deleting the
// subject. A surface that says nothing about fishhawk_audit_complete cannot
// over-claim, but it also cannot tell an operator which check to look for.
func TestAuditCheckWording_SweptSurfacesNameThePublishedCheck(t *testing.T) {
	for _, surface := range sweptAuditWordingSurfaces(t) {
		if !strings.Contains(normalizeAuditWording(surface.text), "fishhawk_audit_complete") {
			t.Errorf("%s no longer names fishhawk_audit_complete — the sweep must correct the claim, not drop the check (#3161)\n\nfull text: %s",
				surface.name, surface.text)
		}
	}
}
