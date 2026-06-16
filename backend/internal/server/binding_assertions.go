package server

import (
	"fmt"
	"strings"
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
