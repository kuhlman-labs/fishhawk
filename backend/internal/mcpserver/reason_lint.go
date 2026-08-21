package mcpserver

import (
	"regexp"
	"strings"
)

// Layer 1 of #2512 (E48.78): the approve-reason lint.
//
// THE PROBLEM. `fishhawk_approve_plan`'s `reason` is delivered to the IMPLEMENT
// agent as binding approval conditions (#558). It is NOT delivered to the plan
// artifact, and the implement agent has no authority over that artifact — it
// cannot retire a criterion, restate one, or change what acceptance will
// validate. So an operator who writes "approved, but criterion AC-3 is wrong,
// ignore it" has addressed the wrong actor: the words land on an agent that
// cannot act on them, acceptance still evaluates AC-3 unchanged, and the run
// fails a criterion the operator already judged bad. That is the "silent prose
// to the wrong actor" failure in the issue title.
//
// WHY A WARNING AND NOT A REFUSAL. An operator may legitimately DISCUSS
// criteria while conditioning something else — "approved; the implementation
// must actually satisfy criterion AC-2, not just compile" is correct usage of
// the reason field and must not be blocked. There is no way to distinguish
// intent from vocabulary, so the lint warns and the approval submits with the
// reason BYTE-UNMODIFIED. A gate here would strand approvals over wording,
// which is the same trade the #2347 acknowledgement ask resolves the same way.
//
// UNEXPORTED BY DESIGN. `export_surface_test.go` is an exhaustive, generated
// baseline of this package's exported top-level identifiers that fails in
// EITHER direction, so a new export would fail it. It is also better design:
// the lint is package-internal and its tests are in-package, so there is no
// reason to widen the API surface for a test.

// reasonLintVocabulary is the plan-artifact vocabulary that triggers the lint.
// Deliberately small and deterministic: a term earns a place here only when an
// operator using it is plausibly trying to CHANGE the plan artifact rather than
// condition the implementation.
//
// Each entry is matched case-insensitively and on WORD BOUNDARIES. The boundary
// requirement is load-bearing for the single-word terms: without it "retire"
// fires on "retirement", and — the case that motivated it — "criterion" would
// fire inside any longer word containing it. A lint that fires on substrings
// trains operators to ignore it, which costs more than the misses.
var reasonLintVocabulary = []string{
	"acceptance criterion",
	"acceptance criteria",
	"criterion",
	"criteria",
	"blocking",
	"non-blocking",
	"retire",
	"demote",
}

// reasonLintPatterns compiles reasonLintVocabulary once at package init. Each
// pattern is `(?i)\b<term>\b` with the term regexp-quoted, so a multi-word term
// matches across its internal single space and a term containing regexp
// metacharacters would still match literally.
//
// `\b` around a term that begins or ends with a non-word character would never
// match; every current term begins and ends with a letter, and the test pins
// that property so a future addition cannot silently become dead vocabulary.
var reasonLintPatterns = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(reasonLintVocabulary))
	for _, term := range reasonLintVocabulary {
		out = append(out, regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(term)+`\b`))
	}
	return out
}()

// lintApprovalReason returns a warning when an approve reason carries
// plan-artifact vocabulary, or "" when it does not.
//
// The warning names the instruments that DO reach the plan artifact, with
// `amend_acceptance_criteria` FIRST. That ordering is the substance of this
// lint, not decoration: #2512's original text predates #2581 and names only
// `fishhawk_revise_plan`, which triggers a FULL REPLAN — a disproportionate and
// slow answer to "this one criterion is wrong". #2581 shipped
// `amend_acceptance_criteria` as an `fishhawk_approve_plan` parameter that
// retires or restates a criterion BY ID at this same gate, without leaving it.
// Pointing an operator at the replan when the amendment exists is the wrong
// advice, so the amendment leads and the replan is the fallback for a plan
// whose problem is bigger than its criteria.
//
// It NEVER refuses: the caller submits the approval with the reason unchanged
// and appends this string to the tool result alongside any other warning.
func lintApprovalReason(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return ""
	}
	var hits []string
	for i, re := range reasonLintPatterns {
		if re.MatchString(reason) {
			hits = append(hits, reasonLintVocabulary[i])
		}
	}
	if len(hits) == 0 {
		return ""
	}
	return "approve-reason lint (#2512): your reason mentions plan-artifact vocabulary (" +
		strings.Join(hits, ", ") + "). The approval was submitted with your reason UNCHANGED — this is a warning, not a refusal. " +
		"Note where `reason` actually goes: it is delivered to the IMPLEMENT agent as binding approval conditions (#558), " +
		"and that agent has NO authority over the plan artifact — it cannot retire, restate, or reweight an acceptance criterion, " +
		"so acceptance will still evaluate the criteria exactly as the plan declares them. " +
		"To change the criteria themselves, use amend_acceptance_criteria on this same fishhawk_approve_plan call (#2581) — " +
		"it retires or restates a criterion BY ID at this gate, with no replan. " +
		"If the plan is wrong beyond its criteria, fishhawk_reject_plan / fishhawk_revise_plan drives a full replan instead."
}
