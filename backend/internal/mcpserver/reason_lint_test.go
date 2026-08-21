package mcpserver

import (
	"regexp"
	"strings"
	"testing"
)

// TestLintApprovalReason_Vocabulary is the vocabulary table. Each firing term
// gets its own case, and each non-firing case names WHY it must stay silent —
// a lint that fires on ordinary approval prose gets ignored, which costs more
// than the misses it prevents.
func TestLintApprovalReason_Vocabulary(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   bool
	}{
		// --- fires ---
		{"acceptance criterion", "approved, but the acceptance criterion about the CLI is wrong", true},
		{"acceptance criteria", "approved; drop the acceptance criteria that need a live forge", true},
		{"bare criterion", "approved — criterion 3 cannot be validated in the sandbox", true},
		{"bare criteria", "approved, ignore the last two criteria", true},
		{"blocking", "approved; AC-2 should not be blocking", true},
		{"non-blocking", "approved, treat AC-2 as non-blocking", true},
		{"retire", "approved — retire AC-4, it is obsolete", true},
		{"demote", "approved; demote the third one", true},
		{"case-insensitive", "APPROVED. RETIRE AC-4.", true},
		{"mid-sentence with punctuation", "approved (criterion: AC-4 is unevaluable).", true},
		{"multiple terms are all reported", "retire the blocking criterion", true},

		// --- stays silent ---
		{"empty", "", false},
		{"whitespace only", "   \n\t ", false},
		{"ordinary approval", "approved, ship it", false},
		{"ordinary conditions", "approved; keep the change under 200 lines and add a table test", false},
		{
			// The word-boundary requirement. Without \b these substrings fire and
			// the lint becomes noise on prose that has nothing to do with the plan
			// artifact.
			name:   "substring of a longer word does not fire",
			reason: "approved; the retirement plan and the demoted-priority backlog are unaffected",
			want:   false,
		},
		{
			// "criterion" inside a longer token, which the plan names explicitly.
			name:   "criterion inside a longer word does not fire",
			reason: "approved; see the multicriterion-solver benchmark before merging",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lintApprovalReason(tc.reason)
			if (got != "") != tc.want {
				t.Fatalf("lintApprovalReason(%q) = %q; fired=%v want fired=%v", tc.reason, got, got != "", tc.want)
			}
		})
	}
}

// TestLintApprovalReason_NamesAmendFirst pins the substance of the lint: the
// instrument that actually reaches the plan artifact must be named, and named
// FIRST. #2512's original text predates #2581 and points only at
// fishhawk_revise_plan — a full replan, a disproportionate answer to one bad
// criterion. An operator sent to the replan when amend_acceptance_criteria
// exists has been given the wrong advice, so the ORDER is asserted, not just
// the presence of both names.
func TestLintApprovalReason_NamesAmendFirst(t *testing.T) {
	got := lintApprovalReason("approved, but retire criterion AC-4")
	if got == "" {
		t.Fatal("lint did not fire on plan-artifact vocabulary")
	}
	amendAt := strings.Index(got, "amend_acceptance_criteria")
	reviseAt := strings.Index(got, "fishhawk_revise_plan")
	if amendAt < 0 {
		t.Errorf("warning does not name amend_acceptance_criteria (#2581), the instrument that reaches the plan at this gate:\n%s", got)
	}
	if reviseAt < 0 {
		t.Errorf("warning does not name fishhawk_revise_plan as the full-replan fallback:\n%s", got)
	}
	if amendAt >= 0 && reviseAt >= 0 && amendAt > reviseAt {
		t.Errorf("warning names fishhawk_revise_plan BEFORE amend_acceptance_criteria — the replan is the fallback, not the first answer:\n%s", got)
	}
	// The warning must explain WHY the reason alone does not work, or the
	// operator has no reason to believe it.
	for _, claim := range []string{"IMPLEMENT agent", "NO authority over the plan artifact"} {
		if !strings.Contains(got, claim) {
			t.Errorf("warning dropped the load-bearing claim %q — without it the advice is unmotivated:\n%s", claim, got)
		}
	}
}

// TestLintApprovalReason_NeverRefuses pins the never-refuses property at the
// level the function can carry it: the lint is a PURE function returning a
// string, so it has no channel through which to refuse — it cannot return an
// error, mutate the reason, or signal a failure. The approval-still-submits
// half of the property lives at the call site and is pinned by
// TestApprovePlan_ReasonLintWarnsAndStillApproves in tools_test.go.
func TestLintApprovalReason_NeverRefuses(t *testing.T) {
	reason := "approved, but retire criterion AC-4 and make AC-2 non-blocking"
	got := lintApprovalReason(reason)
	if got == "" {
		t.Fatal("lint did not fire; nothing to assert")
	}
	// The warning says plainly that this is not a refusal, so an operator
	// reading it does not conclude the approval failed and re-submit.
	if !strings.Contains(got, "not a refusal") {
		t.Errorf("warning does not say it is not a refusal:\n%s", got)
	}
	if !strings.Contains(got, "UNCHANGED") {
		t.Errorf("warning does not tell the operator their reason was submitted unchanged:\n%s", got)
	}
	// The lint is pure: calling it does not alter the reason it was given.
	if reason != "approved, but retire criterion AC-4 and make AC-2 non-blocking" {
		t.Fatal("lintApprovalReason mutated its input")
	}
	// Idempotent — a second call on the same input returns the same warning.
	if again := lintApprovalReason(reason); again != got {
		t.Errorf("lintApprovalReason is not deterministic:\n%q\nvs\n%q", got, again)
	}
}

// TestReasonLintVocabulary_TermsAreWordBoundaryMatchable guards a trap the
// pattern construction sets for future edits. Every term is wrapped in `\b`,
// which is a boundary between a word and a non-word character — so a term that
// BEGINS or ENDS with a non-word character (a leading `-`, a trailing `.`)
// compiles fine and then never matches anything. It would be silently dead
// vocabulary: added in good faith, reviewed, and inert.
func TestReasonLintVocabulary_TermsAreWordBoundaryMatchable(t *testing.T) {
	wordEdge := regexp.MustCompile(`^\w.*\w$|^\w$`)
	for _, term := range reasonLintVocabulary {
		if !wordEdge.MatchString(term) {
			t.Errorf("term %q does not begin AND end with a word character; the \\b wrapper makes it unmatchable (dead vocabulary)", term)
		}
		// Prove it rather than only reasoning about it: every term must fire on
		// a sentence that contains it.
		if lintApprovalReason("approved, "+term+" here") == "" {
			t.Errorf("term %q is in the vocabulary but does not fire on a sentence containing it", term)
		}
	}
	if len(reasonLintPatterns) != len(reasonLintVocabulary) {
		t.Fatalf("compiled %d patterns for %d terms — the two lists must stay 1:1 (the warning reports hits by index)",
			len(reasonLintPatterns), len(reasonLintVocabulary))
	}
}
