package server

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// bindingAssertion is one operator-declared, deterministic binding-assertion
// check (#1171): a typed substring assertion the operator attaches at plan
// approval time so an explicit approval condition becomes machine-checkable
// post-implement. v0 types are:
//
//   - file_contains: Literal must appear (deterministic substring) in the
//     committed content of Path.
//   - test_asserts: same substring primitive, but Path must name a Go test
//     file (`*_test.go`); the type distinction documents intent for the
//     evidence surface, the check is identical.
//
// The Type field is a plain string so a future type adds without a wire-shape
// break (open enum), but validateBindingAssertions rejects any Type outside
// the known set so an operator typo can't silently pass the gate. The wire
// tags (type/path/literal) are byte-identical to the MCP client's
// BindingAssertion and (slice 2) the runner's upload.BindingAssertion, so the
// declaration round-trips approve-request → audit payload → prompt-response →
// runner decode unchanged.
type bindingAssertion struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	Literal string `json:"literal"`
}

// Known binding-assertion types (open enum). Adding a type here recognizes it
// at declaration time; the type field stays a plain string on the wire.
const (
	bindingAssertFileContains = "file_contains"
	bindingAssertTestAsserts  = "test_asserts"
)

// validateBindingAssertions checks every declared assertion against the v0
// contract and returns a descriptive error on the first violation (the handler
// 400s validation_failed with the message). The rules:
//
//   - Type must be one of the known set (file_contains | test_asserts) — an
//     unknown type is an operator typo, rejected rather than silently passed.
//   - Path must be a clean repo-relative path (no leading '/', no '..'
//     traversal), reusing isRepoRelativePath's semantics so a declared path
//     can name a real committed scope.files entry.
//   - Literal must be non-empty (an empty substring would match every file).
//   - For test_asserts, Path must end in `_test.go`.
//
// An empty (or nil) slice is valid — it is the byte-identical no-declaration
// path: the handler omits the audit key and the prompt-response field.
func validateBindingAssertions(assertions []bindingAssertion) error {
	for i, a := range assertions {
		switch a.Type {
		case bindingAssertFileContains, bindingAssertTestAsserts:
		default:
			return fmt.Errorf("binding_assertions[%d].type %q is not a recognized type (want %s or %s)",
				i, a.Type, bindingAssertFileContains, bindingAssertTestAsserts)
		}
		if a.Path == "" {
			return fmt.Errorf("binding_assertions[%d].path is required", i)
		}
		if !isRepoRelativePath(a.Path) {
			return fmt.Errorf("binding_assertions[%d].path %q must be repo-relative (no leading '/' or '..' segment)", i, a.Path)
		}
		if a.Literal == "" {
			return fmt.Errorf("binding_assertions[%d].literal is required (an empty substring matches every file)", i)
		}
		if a.Type == bindingAssertTestAsserts && !strings.HasSuffix(a.Path, "_test.go") {
			return fmt.Errorf("binding_assertions[%d].path %q must end in _test.go for a test_asserts assertion", i, a.Path)
		}
	}
	return nil
}

// Named weak-assertion heuristics (#2501 proposal 3). Each name is emitted in
// its warning message so the operator can tell the three apart at a glance —
// and so a future surface can key off the name without re-parsing the prose.
const (
	// warnKindPathNotInScope: the asserted path is absent from the approved
	// plan's scope.files, so the implement pass is not expected to touch it
	// at all and the assertion cannot be satisfied by a compliant pass.
	warnKindPathNotInScope = "path-not-in-scope"
	// warnKindLiteralAbsent: the literal appears nowhere in the approved
	// plan's text, so nothing in the plan promises to produce it.
	warnKindLiteralAbsent = "literal-absent-from-plan"
	// warnKindLiteralOtherPath: the literal DOES appear in the plan, but
	// every plan sentence mentioning it names a DIFFERENT scope.files path
	// than the asserted one — the #2501 case verbatim.
	warnKindLiteralOtherPath = "literal-paired-with-another-path"
)

// warnBindingAssertions evaluates three cheap, named heuristics against the
// approved plan when an approve carries binding_assertions (#2501 proposal 3)
// and returns one human-readable warning per firing heuristic, in assertion
// order. It exists because a binding assertion is operator-authored prose
// compiled into a machine gate: a path typo or a literal paired with the wrong
// file is invisible at the approval gate and only surfaces post-implement, at
// which point it has already cost a full implement pass (run 85f52d88, #2501).
//
// It is ADVISORY ONLY and never a refusal. An assertion about content that
// does not exist yet is the whole point of the feature, so every heuristic here
// is a heuristic about the PLAN's text rather than a fact about the tree, and a
// firing warning is frequently correct-but-intentional. The caller records the
// warnings on the approval audit payload and returns them on the approve 200
// body; the approval itself proceeds unchanged.
//
// It FAILS OPEN in every indeterminate state — a nil plan (artifact absent, an
// unconfigured artifact repository, or a load error the caller swallowed), an
// empty assertion list, or a plan with an empty scope.files (W1 only) yields no
// warnings. An advisory surface must never turn a working approval into a
// diagnostic, so silence is always the safe answer.
//
// The three heuristics:
//
//   - W1 path-not-in-scope: a.Path is not one of the plan's scope.files paths.
//     Evaluated against the UNION of the flat plan's scope.files and every
//     decomposition sub-plan's scope, so a decomposed plan whose slices own the
//     asserted path does not draw a spurious warning.
//   - W2 literal-absent-from-plan: a.Literal appears nowhere in the plan's
//     text (summary + approach step descriptions + verification.test_strategy).
//   - W3 literal-paired-with-another-path: a.Literal appears in the plan text,
//     but no sentence mentioning it names a.Path, while at least one such
//     sentence names a different scope.files path. The message names those
//     paths. This is the observed #2501 shape: the literal `checks_unresolved`
//     lived in an approach step naming drive_test.go while the assertion named
//     policy_reeval_test.go.
//
// W2 and W3 are mutually exclusive by construction (absent vs present); W1 is
// independent and can fire alongside either.
func warnBindingAssertions(assertions []bindingAssertion, approvedPlan *plan.Plan) []string {
	if len(assertions) == 0 || approvedPlan == nil {
		return nil
	}
	scopePaths := planScopePaths(approvedPlan)
	sentences := planSentences(approvedPlan)
	planText := strings.Join(sentences, "\n")

	var warnings []string
	for i, a := range assertions {
		// W1 needs a non-empty scope to be meaningful: a plan with no
		// scope.files at all is indeterminate, not a plan that excludes
		// every asserted path.
		if a.Path != "" && len(scopePaths) > 0 && !containsExact(scopePaths, a.Path) {
			warnings = append(warnings, fmt.Sprintf(
				"binding_assertions[%d] [%s]: path %q is not in the approved plan's scope.files, so the implement pass is not expected to touch it; the assertion cannot be satisfied by a compliant pass",
				i, warnKindPathNotInScope, a.Path))
		}
		if a.Literal == "" {
			continue
		}
		if !strings.Contains(planText, a.Literal) {
			warnings = append(warnings, fmt.Sprintf(
				"binding_assertions[%d] [%s]: literal %q appears nowhere in the approved plan (summary, approach steps, verification.test_strategy), so nothing in the approved plan promises to produce it",
				i, warnKindLiteralAbsent, a.Literal))
			continue
		}
		if others := pathsPairedWithLiteral(sentences, scopePaths, a.Literal, a.Path); len(others) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"binding_assertions[%d] [%s]: literal %q appears in the approved plan but every plan sentence mentioning it names a different scope file (%s), not the asserted %q",
				i, warnKindLiteralOtherPath, a.Literal, strings.Join(others, ", "), a.Path))
		}
	}
	return warnings
}

// planScopePaths returns the union of the plan's own scope.files paths and
// every decomposition sub-plan's scope paths, deduped in first-seen order. The
// union matters for W1: on a decomposed plan the parent's scope.files is the
// overall set but a slice may carry the asserted path in its own scope, and an
// assertion naming a slice-owned file is correctly declared.
func planScopePaths(p *plan.Plan) []string {
	seen := make(map[string]bool)
	var paths []string
	add := func(files []plan.ScopeFile) {
		for _, f := range files {
			if f.Path == "" || seen[f.Path] {
				continue
			}
			seen[f.Path] = true
			paths = append(paths, f.Path)
		}
	}
	add(p.Scope.Files)
	if p.Decomposition != nil {
		for _, sp := range p.Decomposition.SubPlans {
			if sp.Scope != nil {
				add(sp.Scope.Files)
			}
		}
	}
	return paths
}

// planSentences splits the plan's operator-readable text — summary, each
// approach step's description, and verification.test_strategy — into sentences.
// The split is deliberately crude (newlines plus `.`/`!`/`?` boundaries): the
// unit only needs to be small enough that a literal and a path co-occurring in
// it is evidence they were written about each other, and large enough that a
// step naming its file once at the front still carries that pairing.
func planSentences(p *plan.Plan) []string {
	var texts []string
	if p.Summary != "" {
		texts = append(texts, p.Summary)
	}
	for _, step := range p.Approach {
		if step.Description != "" {
			texts = append(texts, step.Description)
		}
	}
	if p.Verification.TestStrategy != "" {
		texts = append(texts, p.Verification.TestStrategy)
	}

	var sentences []string
	for _, text := range texts {
		for _, line := range strings.Split(text, "\n") {
			for _, s := range splitSentences(line) {
				if s = strings.TrimSpace(s); s != "" {
					sentences = append(sentences, s)
				}
			}
		}
	}
	return sentences
}

// splitSentences breaks one line on `.`, `!` and `?` terminators that are
// followed by whitespace or end-of-line. Requiring the trailing whitespace
// keeps a path like `a/b_test.go` — whose dot is mid-token — in one piece,
// which is load-bearing for W3: splitting inside a filename would separate the
// path from the literal it was written next to.
func splitSentences(line string) []string {
	var out []string
	start := 0
	for i, r := range line {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		next := i + 1
		if next < len(line) && line[next] != ' ' && line[next] != '\t' {
			continue
		}
		out = append(out, line[start:next])
		start = next
	}
	if start < len(line) {
		out = append(out, line[start:])
	}
	return out
}

// pathsPairedWithLiteral implements W3's judgment. It scans every sentence
// containing the literal: if ANY of them mentions assertedPath the pairing is
// correct and it returns nil (quiet). Otherwise it returns the distinct scope
// paths those sentences DO mention, sorted for a deterministic message. An
// empty return means quiet — either the literal never co-occurs with a scope
// path at all (the plan mentions it path-lessly, which W3 deliberately does not
// warn about), or the pairing was right.
func pathsPairedWithLiteral(sentences, scopePaths []string, literal, assertedPath string) []string {
	seen := make(map[string]bool)
	var others []string
	for _, s := range sentences {
		if !strings.Contains(s, literal) {
			continue
		}
		if assertedPath != "" && mentionsPath(s, assertedPath) {
			return nil
		}
		for _, p := range scopePaths {
			if p == assertedPath || seen[p] {
				continue
			}
			if mentionsPath(s, p) {
				seen[p] = true
				others = append(others, p)
			}
		}
	}
	sort.Strings(others)
	return others
}

// mentionsPath reports whether a plan sentence names the given scope path,
// matching either the full repo-relative path or its basename. Plan prose
// routinely names a file by basename ("…paired with drive_test.go"), so
// basename matching is what makes W3 fire on the real #2501 shape; the
// basename must carry a dot and be longer than three characters so a short or
// extension-less directory name cannot match half a sentence.
func mentionsPath(sentence, scopePath string) bool {
	if strings.Contains(sentence, scopePath) {
		return true
	}
	base := path.Base(scopePath)
	if len(base) > 3 && strings.Contains(base, ".") {
		return strings.Contains(sentence, base)
	}
	return false
}

// containsExact reports whether values holds an exact match for want.
func containsExact(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
